package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// healthBody 拉一次 /healthz，返回解析后的 JSON 与状态码。
func healthBody(t *testing.T, url, token string) (map[string]any, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/healthz", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 /healthz 响应: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("解析 /healthz 响应 %q: %v", raw, err)
	}
	return body, resp.StatusCode
}

// 探活口不只是能连通：匿名请求不该看到版本与连接数。
//
// 这类信息在暴露到内网/反代之后就是免费的侦察材料（版本对应已知漏洞），
// 而探针本身又确实不该配凭据，所以用「同一个 200、两档字段」来分。
func TestHealthzHidesDetailsWithoutToken(t *testing.T) {
	ts := newTestServer(t)

	anon, code := healthBody(t, ts.URL, "")
	if code != http.StatusOK {
		t.Fatalf("匿名 healthz 状态码 = %d，想要 200（探针不该被挡住）", code)
	}
	if anon["status"] != "ok" {
		t.Fatalf("匿名 healthz status = %v，想要 ok", anon["status"])
	}
	for _, key := range []string{"version", "protocol_version", "uptime_seconds", "connections"} {
		if _, ok := anon[key]; ok {
			t.Fatalf("匿名 healthz 不应包含 %q，实际返回 %v", key, anon)
		}
	}

	authed, code := healthBody(t, ts.URL, testToken)
	if code != http.StatusOK {
		t.Fatalf("带令牌 healthz 状态码 = %d，想要 200", code)
	}
	if authed["version"] != "test" {
		t.Fatalf("带令牌 healthz version = %v，想要 test", authed["version"])
	}
	if _, ok := authed["connections"]; !ok {
		t.Fatalf("带令牌 healthz 应包含 connections，实际 %v", authed)
	}

	// 伪造令牌等同匿名，不能因为「带了 Authorization 头」就多给字段。
	bad, _ := healthBody(t, ts.URL, "wrong-token")
	if _, ok := bad["version"]; ok {
		t.Fatalf("错误令牌不应看到版本信息，实际 %v", bad)
	}
}
