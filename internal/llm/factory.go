package llm

import (
	"fmt"
	"net/http"

	"github.com/kennethfan/jiaoag/internal/config"
)

// New 按配置构造一家供应商，并套上固定顺序的装饰器：
//
//	retry(timeout(adapter))
//
// 适配器只做协议翻译；重试与超时只在装饰器里实现一次，两家共享同一套行为。
// 返回的 Provider 一定同时实现 Streamer（两家适配器都支持流式）。
func New(cfg config.Provider) (Provider, error) {
	switch cfg.Type {
	case "openai", "anthropic":
	default:
		// 先校验类型，避免配置写错时被 api_key 的报错盖住。
		return nil, fmt.Errorf("llm: 未支持的供应商类型 %q（供应商 %s）", cfg.Type, cfg.Name)
	}

	key, err := cfg.APIKey.Resolve()
	if err != nil {
		return nil, fmt.Errorf("llm: 解析供应商 %s 的 api_key 失败：%w", cfg.Name, err)
	}

	// 超时完全由装饰器负责（首块 + 空闲），http.Client 不再叠加总超时。
	client := &http.Client{}

	var base Provider
	switch cfg.Type {
	case "openai":
		base = newOpenAI(cfg.Name, cfg.BaseURL, key, cfg.Model, client)
	case "anthropic":
		base = newAnthropic(cfg.Name, cfg.BaseURL, key, cfg.Model, client)
	}

	// 限速放在重试之内：每次重试都要重新拿配额，否则 429 的重试会成倍放大。
	if cfg.RPM > 0 || cfg.TPM > 0 {
		base = newRateProvider(base, cfg.RPM, cfg.TPM)
	}
	return newRetryProvider(newTimeoutProvider(base, cfg.Timeout()), cfg.Retries()), nil
}
