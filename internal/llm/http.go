package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// maxErrorBody 是读取错误响应体的上限：错误信息够用即可，避免被超大响应拖住。
const maxErrorBody = 64 << 10

// doJSON 发送请求并统一处理传输层错误与非 2xx 状态。
//
// 适配器只负责拼请求与解响应，错误分类都收敛在这里和 errors.go，
// 保证两家（以及未来第三家）的错误行为一致。
func doJSON(ctx context.Context, client *http.Client, provider string, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		// 调用方取消优先原样返回，重试装饰器据此判定「不再重试」。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("llm: %s 网络请求失败：%w: %w", provider, ErrTransient, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr != nil {
		body = nil
	}
	return nil, classifyError(provider, resp.StatusCode, resp.Header.Get("Retry-After"), body)
}

// newJSONRequest 构造 POST 请求；body 由调用方负责 marshal。
func newJSONRequest(ctx context.Context, url string, body []byte, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: 构造请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}
