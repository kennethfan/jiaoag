package llm

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/config"
)

func TestNewRejectsUnknownType(t *testing.T) {
	_, err := New(config.Provider{Name: "p", Type: "gemini", BaseURL: "https://example.com", Model: "m"})
	if err == nil {
		t.Fatal("未知 type 应返回错误")
	}
	if !strings.Contains(err.Error(), "未支持") {
		t.Fatalf("错误信息应说明类型未支持：%v", err)
	}
}

func TestNewResolvesAPIKey(t *testing.T) {
	t.Setenv("JIAOAG_TEST_KEY", "sk-from-env")
	_, err := New(config.Provider{
		Name:    "p",
		Type:    "openai",
		BaseURL: "https://example.com",
		Model:   "m",
		APIKey:  "$JIAOAG_TEST_KEY",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = New(config.Provider{
		Name:    "p",
		Type:    "openai",
		BaseURL: "https://example.com",
		Model:   "m",
		APIKey:  "$JIAOAG_NO_SUCH_KEY_FOR_TEST",
	})
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("密钥无法解析时应报错并指出 api_key：%v", err)
	}
}

func TestNewWiresDecoratorChain(t *testing.T) {
	t.Setenv("JIAOAG_TEST_KEY", "sk-env")
	timeoutSeconds, retries := 20, 0
	cfg := config.Provider{
		Name:           "p",
		Type:           "openai",
		BaseURL:        "https://example.com/v1",
		Model:          "gpt-test",
		APIKey:         "$JIAOAG_TEST_KEY",
		TimeoutSeconds: &timeoutSeconds,
		MaxRetries:     &retries,
	}

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 顺序必须是 retry(timeout(adapter))。
	rp, ok := p.(*retryProvider)
	if !ok {
		t.Fatalf("最外层应为 *retryProvider，实际 %T", p)
	}
	if rp.attempts != 1 {
		t.Fatalf("max_retries=0 应只尝试 1 次，实际 %d", rp.attempts)
	}
	tp, ok := rp.next.(*timeoutProvider)
	if !ok {
		t.Fatalf("第二层应为 *timeoutProvider，实际 %T", rp.next)
	}
	if tp.firstByte.Seconds() != 20 {
		t.Fatalf("首块超时 = %s，期望 20s", tp.firstByte)
	}
	oa, ok := tp.next.(*openaiAdapter)
	if !ok {
		t.Fatalf("最内层应为 *openaiAdapter，实际 %T", tp.next)
	}
	if oa.apiKey != "sk-env" {
		t.Fatalf("api key 未解析：%q", oa.apiKey)
	}
	if oa.model != "gpt-test" || oa.baseURL != "https://example.com/v1" {
		t.Fatalf("适配器配置未传递：%+v", oa)
	}
	if _, ok := p.(Streamer); !ok {
		t.Fatal("组合后的 Provider 必须仍然实现 Streamer")
	}
}

func TestNewSupportsAnthropic(t *testing.T) {
	p, err := New(config.Provider{
		Name:    "a",
		Type:    "anthropic",
		BaseURL: "https://api.anthropic.com",
		Model:   "claude-test",
		APIKey:  "sk-ant-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rp := p.(*retryProvider)
	tp := rp.next.(*timeoutProvider)
	aa, ok := tp.next.(*anthropicAdapter)
	if !ok {
		t.Fatalf("最内层应为 *anthropicAdapter，实际 %T", tp.next)
	}
	if aa.model != "claude-test" {
		t.Fatalf("model = %q", aa.model)
	}
	if rp.attempts != config.DefaultProviderMaxRetries+1 {
		t.Fatalf("默认尝试次数 = %d，期望 %d", rp.attempts, config.DefaultProviderMaxRetries+1)
	}
}
