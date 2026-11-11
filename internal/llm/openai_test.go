package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIChatRequestAndResponseShape(t *testing.T) {
	var got map[string]any
	var gotPath, gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		got = readJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"model": "gpt-test",
			"choices": [{
				"message": {
					"content": "好的",
					"tool_calls": [{"id": "call_2", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"b.go\"}"}}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 7}
		}`)
	}))
	defer srv.Close()

	a := newOpenAI("test", srv.URL, "sk-test", "gpt-default", srv.Client())
	req := Request{
		Model:  "gpt-test",
		System: "你是助手",
		Messages: []Message{
			TextMessage(RoleUser, "读一下 a.go"),
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolUse, ToolCallID: "call_1", ToolName: "read_file", Args: json.RawMessage(`{"path":"a.go"}`)},
			}},
			ToolResultMessage(Block{Kind: BlockToolResult, ToolCallID: "call_1", Result: "package a"}),
		},
		Tools:       []ToolSpec{{Name: "read_file", Description: "读取文件", Schema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens:   256,
		Temperature: f64(0.2),
		Stop:        []string{"END"},
	}

	resp, err := a.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// —— 请求形状 ——
	if gotPath != "/chat/completions" {
		t.Fatalf("路径 = %q，期望 /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if got["model"] != "gpt-test" {
		t.Fatalf("model = %v", got["model"])
	}
	if _, ok := got["stream"]; ok {
		t.Fatal("非流式请求不应带 stream 字段")
	}
	if got["max_tokens"].(float64) != 256 {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", got["tool_choice"])
	}

	messages := got["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages 长度 = %d，期望 4（system + user + assistant + tool）", len(messages))
	}
	sys := messages[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "你是助手" {
		t.Fatalf("system 消息不正确：%v", sys)
	}
	assistant := messages[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool_calls[0] = %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "read_file" || fn["arguments"] != `{"path":"a.go"}` {
		t.Fatalf("function = %v", fn)
	}
	toolMsg := messages[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "package a" {
		t.Fatalf("工具结果消息不正确：%v", toolMsg)
	}
	toolDef := got["tools"].([]any)[0].(map[string]any)
	if toolDef["type"] != "function" {
		t.Fatalf("tools[0].type = %v", toolDef["type"])
	}
	if toolDef["function"].(map[string]any)["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("tools[0].function.parameters = %v", toolDef["function"])
	}

	// —— 响应映射 ——
	if resp.StopReason != StopToolUse {
		t.Fatalf("StopReason = %q，期望 tool_use", resp.StopReason)
	}
	if resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("Usage = %+v", resp.Usage)
	}
	if len(resp.Message.Blocks) != 2 {
		t.Fatalf("Blocks 数 = %d，期望 2（text + tool_use）", len(resp.Message.Blocks))
	}
	if resp.Message.Blocks[0].Kind != BlockText || resp.Message.Blocks[0].Text != "好的" {
		t.Fatalf("文本块 = %+v", resp.Message.Blocks[0])
	}
	use := resp.Message.Blocks[1]
	if use.Kind != BlockToolUse || use.ToolCallID != "call_2" || use.ToolName != "read_file" || string(use.Args) != `{"path":"b.go"}` {
		t.Fatalf("工具块 = %+v", use)
	}
}

func TestOpenAIOmitsEmptyTools(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readJSONBody(t, r)
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	a := newOpenAI("test", srv.URL, "k", "m", srv.Client())
	if _, err := a.Chat(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("tools 为空时不应发送该字段（兼容网关会拒绝空数组）")
	}
	if _, ok := got["tool_choice"]; ok {
		t.Fatal("tools 为空时不应发送 tool_choice")
	}
}

func TestOpenAIStreamTextAndToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readJSONBody(t, r)
		if body["stream"] != true {
			t.Errorf("流式请求应带 stream=true，实际 %v", body["stream"])
		}
		opts, ok := body["stream_options"].(map[string]any)
		if !ok || opts["include_usage"] != true {
			t.Errorf("stream_options 应要求 include_usage，实际 %v", body["stream_options"])
		}
		writeSSE(t, w,
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"，世界\"},\"finish_reason\":null}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]},\"finish_reason\":null}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a.go\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34}}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	a := newOpenAI("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{Model: "m"})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if text := joinedText(chunks); text != "你好，世界" {
		t.Fatalf("文本 = %q", text)
	}
	calls := mergeToolCalls(chunks)
	if len(calls) != 1 {
		t.Fatalf("工具调用数 = %d，期望 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "read_file" || calls[0].Args != `{"path":"a.go"}` {
		t.Fatalf("合并后的工具调用 = %+v", calls[0])
	}
	if n := countDone(chunks); n != 1 {
		t.Fatalf("Done 出现 %d 次，期望 1", n)
	}
	if n := countUsage(chunks); n != 1 {
		t.Fatalf("usage 出现 %d 次，期望 1", n)
	}
	last := finalChunk(chunks)
	if last.Usage == nil || last.Usage.PromptTokens != 12 || last.Usage.CompletionTokens != 34 {
		t.Fatalf("最终 usage = %+v", last.Usage)
	}
	if last.StopReason != StopToolUse {
		t.Fatalf("StopReason = %q", last.StopReason)
	}
}

func TestOpenAIStreamMergesParallelToolCallsByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"function\":{\"name\":\"a\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"c1\",\"function\":{\"name\":\"b\",\"arguments\":\"{\\\"y\\\":\"}}]}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"2}\"}}]}}]}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	a := newOpenAI("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	calls := mergeToolCalls(chunks)
	if len(calls) != 2 {
		t.Fatalf("工具调用数 = %d，期望 2", len(calls))
	}
	if calls[0].ID != "c0" || calls[0].Args != `{"x":1}` {
		t.Fatalf("calls[0] = %+v", calls[0])
	}
	if calls[1].ID != "c1" || calls[1].Args != `{"y":2}` {
		t.Fatalf("calls[1] = %+v", calls[1])
	}
}

func TestOpenAIChatErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		want       error
		wantAfter  string
	}{
		{"限流", http.StatusTooManyRequests, "7", `{"error":{"message":"slow down"}}`, ErrRateLimited, "7s"},
		{"鉴权", http.StatusUnauthorized, "", `{"error":{"message":"bad key"}}`, ErrAuth, ""},
		{"禁止", http.StatusForbidden, "", `{"error":{"message":"no"}}`, ErrAuth, ""},
		{"请求错误", http.StatusBadRequest, "", `{"error":{"message":"bad"}}`, ErrBadRequest, ""},
		{"上下文超限", http.StatusBadRequest, "", `{"error":{"message":"maximum context length is 8192"}}`, ErrContextOverflow, ""},
		{"服务端错误", http.StatusInternalServerError, "", `oops`, ErrTransient, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			a := newOpenAI("test", srv.URL, "k", "m", srv.Client())
			_, err := a.Chat(context.Background(), Request{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.want)
			}
			if tc.wantAfter != "" {
				var limited *RateLimitError
				if !errors.As(err, &limited) {
					t.Fatalf("错误不是 *RateLimitError：%v", err)
				}
				if limited.RetryAfter.String() != tc.wantAfter {
					t.Fatalf("RetryAfter = %s，期望 %s", limited.RetryAfter, tc.wantAfter)
				}
			}
		})
	}
}

func TestOpenAIStreamAbortsOnCallbackError(t *testing.T) {
	sentinel := errors.New("调用方要求中止")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"第一块\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"第二块\"}}]}\n\n",
			"data: [DONE]\n\n",
		)
	}))
	defer srv.Close()

	a := newOpenAI("test", srv.URL, "k", "m", srv.Client())
	var seen int
	err := a.ChatStream(context.Background(), Request{}, func(c Chunk) error {
		if c.TextDelta != "" {
			seen++
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ChatStream 应原样返回回调错误，实际 %v", err)
	}
	if seen != 1 {
		t.Fatalf("回调被调用 %d 次，期望在执行中止前只调用 1 次", seen)
	}
}

func TestOpenAIStreamReturnsUsageAtEOFWithoutDoneMarker(t *testing.T) {
	// 某些网关不发 [DONE]：仍要给出唯一的 Done 信号。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"结束\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer srv.Close()

	a := newOpenAI("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if n := countDone(chunks); n != 1 {
		t.Fatalf("Done 出现 %d 次，期望 1", n)
	}
	if finalChunk(chunks).StopReason != StopEnd {
		t.Fatalf("StopReason = %q", finalChunk(chunks).StopReason)
	}
}

func TestOpenAIEndpointTrimsTrailingSlash(t *testing.T) {
	a := newOpenAI("t", "https://api.example.com/v1/", "", "m", http.DefaultClient)
	if got := a.endpoint(); got != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("endpoint = %q", got)
	}
	if !strings.HasSuffix(a.endpoint(), "/chat/completions") {
		t.Fatalf("endpoint = %q", a.endpoint())
	}
}
