package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ErrModelsUnsupported 表示当前供应商/适配器不提供模型列表。
var ErrModelsUnsupported = errors.New("llm: 供应商不支持模型列表")

// maxModelsBody 限制模型列表响应体，避免异常端点把内存打满。
const maxModelsBody = 2 << 20

// ModelLister 是可选能力：能列出该供应商当前可用的模型 ID。
//
// 它不是 Provider 的一部分——老适配器与测试桩不需要为了聊天能力实现
// 一个用不到的列表接口。
type ModelLister interface {
	Models(ctx context.Context) ([]string, error)
}

func (a *openaiAdapter) Models(ctx context.Context) ([]string, error) {
	url := strings.TrimRight(a.baseURL, "/") + "/models"
	return fetchModels(ctx, a.client, a.name, url, map[string]string{
		"Authorization": "Bearer " + a.apiKey,
	})
}

func (a *anthropicAdapter) Models(ctx context.Context) ([]string, error) {
	base := strings.TrimRight(a.baseURL, "/")
	url := base + "/v1/models"
	if strings.HasSuffix(base, "/v1") {
		url = base + "/models"
	}
	return fetchModels(ctx, a.client, a.name, url, map[string]string{
		"x-api-key":         a.apiKey,
		"anthropic-version": anthropicVersion,
	})
}

// fetchModels 读取 OpenAI 风格的 {data:[{id}]} 模型列表。
func fetchModels(ctx context.Context, client *http.Client, provider, url string, headers map[string]string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("llm: 构造模型列表请求失败：%w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := doJSON(ctx, client, provider, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, fmt.Errorf("llm: 读取模型列表失败：%w: %w", ErrTransient, err)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("llm: 解析模型列表失败：%w", ErrBadRequest)
	}
	seen := make(map[string]struct{}, len(payload.Data))
	out := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("llm: %s 没有返回可用模型", provider)
	}
	return out, nil
}

func (p *retryProvider) Models(ctx context.Context) ([]string, error) {
	lister, ok := p.next.(ModelLister)
	if !ok {
		return nil, ErrModelsUnsupported
	}
	return lister.Models(ctx)
}

func (p *timeoutProvider) Models(ctx context.Context) ([]string, error) {
	lister, ok := p.next.(ModelLister)
	if !ok {
		return nil, ErrModelsUnsupported
	}
	return lister.Models(ctx)
}

func (r *rateProvider) Models(ctx context.Context) ([]string, error) {
	lister, ok := r.provider.(ModelLister)
	if !ok {
		return nil, ErrModelsUnsupported
	}
	// 模型列表是轻量 GET，不消耗 token 桶；只受 RPM 约束。
	if err := r.rpm.wait(ctx, 1); err != nil {
		return nil, err
	}
	return lister.Models(ctx)
}

// Models 在 key 池上逐个尝试：列模型也会遇到鉴权/额度/限流问题，
// 复用与聊天相同的冷却逻辑，避免每次都从坏 key 开始。
func (p *keyedProvider) Models(ctx context.Context) ([]string, error) {
	tried := make(map[int]bool, len(p.keys))
	var lastErr error
	for len(tried) < len(p.keys) {
		idx := p.pick(tried)
		if idx < 0 {
			break
		}
		tried[idx] = true
		lister, ok := p.keys[idx].(ModelLister)
		if !ok {
			lastErr = ErrModelsUnsupported
			continue
		}
		models, err := lister.Models(ctx)
		if err == nil {
			return models, nil
		}
		lastErr = err
		if !switchable(err) {
			return nil, err
		}
		p.evict(idx, err)
	}
	if lastErr == nil {
		lastErr = ErrModelsUnsupported
	}
	return nil, p.allFailed(lastErr)
}
