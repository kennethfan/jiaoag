package llm

import (
	"fmt"
	"net/http"

	"github.com/kennethfan/jiaoag/internal/config"
)

// New 按配置构造一家供应商，并套上固定顺序的装饰器：
//
//	retry(timeout(rate(keyed(adapter))))
//
// 适配器只做协议翻译；轮询 / 限速 / 超时 / 重试都只在装饰器里实现一次，两家共享同一套行为。
// 返回的 Provider 一定同时实现 Streamer（两家适配器都支持流式）。
func New(cfg config.Provider) (Provider, error) {
	switch cfg.Type {
	case "openai", "anthropic":
	default:
		// 先校验类型，避免配置写错时被 api_key 的报错盖住。
		return nil, fmt.Errorf("llm: 未支持的供应商类型 %q（供应商 %s）", cfg.Type, cfg.Name)
	}

	keys, err := resolveAPIKeys(cfg)
	if err != nil {
		return nil, fmt.Errorf("llm: 解析供应商 %s 的 api_key 失败：%w", cfg.Name, err)
	}

	// 超时完全由装饰器负责（首块 + 空闲），http.Client 不再叠加总超时。
	client := &http.Client{}

	adapters := make([]Provider, 0, len(keys))
	for _, key := range keys {
		switch cfg.Type {
		case "openai":
			adapters = append(adapters, newOpenAI(cfg.Name, cfg.BaseURL, key, cfg.Model, client))
		case "anthropic":
			adapters = append(adapters, newAnthropic(cfg.Name, cfg.BaseURL, key, cfg.Model, client))
		}
	}

	var base Provider = adapters[0]
	if len(adapters) > 1 {
		base = newKeyedProvider(cfg.Name, adapters)
	}

	// 限速放在重试之内：每次重试都要重新拿配额，否则 429 的重试会成倍放大。
	// 多把 key 共用同一个配额桶：rpm/tpm 描述的是这家供应商的整体约束，
	// 不是单把 key 的约束。
	if cfg.RPM > 0 || cfg.TPM > 0 {
		base = newRateProvider(base, cfg.RPM, cfg.TPM)
	}
	return newRetryProvider(newTimeoutProvider(base, cfg.Timeout()), cfg.Retries()), nil
}

// resolveAPIKeys 解析 api_keys 列表（优先）或单个 api_key，按顺序去重。
func resolveAPIKeys(cfg config.Provider) ([]string, error) {
	secrets := cfg.APIKeys
	if len(secrets) == 0 {
		secrets = []config.Secret{cfg.APIKey}
	}
	keys := make([]string, 0, len(secrets))
	seen := make(map[string]bool, len(secrets))
	for i, secret := range secrets {
		key, err := secret.Resolve()
		if err != nil {
			if len(cfg.APIKeys) > 0 {
				return nil, fmt.Errorf("api_keys[%d]: %w", i, err)
			}
			return nil, fmt.Errorf("api_key: %w", err)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("没有可用的 api_key")
	}
	return keys, nil
}
