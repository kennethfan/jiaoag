package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicChatRequestAndResponseShape(t *testing.T) {
	var (
		got     map[string]any
		gotPath string
		apiKey  string
		version string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		apiKey = r.Header.Get("x-api-key")
		version = r.Header.Get("anthropic-version")
		got = readJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"model": "claude-test",
			"content": [
				{"type": "text", "text": "先看看"},
				{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "b.go"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 11, "output_tokens": 13}
		}`)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "sk-ant", "claude-default", srv.Client())
	req := Request{
		System: "你是助手",
		Messages: []Message{
			TextMessage(RoleUser, "读一下 a.go"),
			{Role: RoleAssistant, Blocks: []Block{
				{Kind: BlockToolUse, ToolCallID: "toolu_0", ToolName: "read_file", Args: json.RawMessage(`{"path":"a.go"}`)},
			}},
			ToolResultMessage(Block{Kind: BlockToolResult, ToolCallID: "toolu_0", Result: "package a", IsError: true}),
		},
		Tools: []ToolSpec{{Name: "read_file", Description: "读取文件", Schema: json.RawMessage(`{"type":"object"}`)}},
		// 故意不给 MaxTokens：Anthropic 必填，适配器应补默认值。
	}

	resp, err := a.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// —— 请求形状 ——
	if gotPath != "/v1/messages" {
		t.Fatalf("路径 = %q，期望 /v1/messages", gotPath)
	}
	if apiKey != "sk-ant" {
		t.Fatalf("x-api-key = %q", apiKey)
	}
	if version != anthropicVersion {
		t.Fatalf("anthropic-version = %q，期望 %q", version, anthropicVersion)
	}
	if got["system"] != "你是助手" {
		t.Fatalf("system 应为顶层字段，实际 %v", got["system"])
	}
	if got["max_tokens"].(float64) != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens = %v，期望默认 %d", got["max_tokens"], defaultAnthropicMaxTokens)
	}
	if got["model"] != "claude-default" {
		t.Fatalf("model 应回落到配置值，实际 %v", got["model"])
	}

	messages := got["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages 长度 = %d，期望 3", len(messages))
	}
	if messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages[0] = %v", messages[0])
	}
	assistantBlocks := messages[1].(map[string]any)["content"].([]any)
	useBlock := assistantBlocks[0].(map[string]any)
	if useBlock["type"] != "tool_use" || useBlock["id"] != "toolu_0" || useBlock["name"] != "read_file" {
		t.Fatalf("assistant 工具块 = %v", useBlock)
	}
	if useBlock["input"].(map[string]any)["path"] != "a.go" {
		t.Fatalf("tool_use.input 应为 JSON 对象，实际 %v", useBlock["input"])
	}
	resultBlocks := messages[2].(map[string]any)["content"].([]any)
	resultBlock := resultBlocks[0].(map[string]any)
	if resultBlock["type"] != "tool_result" || resultBlock["tool_use_id"] != "toolu_0" {
		t.Fatalf("tool_result 块 = %v", resultBlock)
	}
	if resultBlock["is_error"] != true {
		t.Fatalf("is_error 未传递：%v", resultBlock["is_error"])
	}
	toolDef := got["tools"].([]any)[0].(map[string]any)
	if toolDef["name"] != "read_file" {
		t.Fatalf("tools[0] = %v", toolDef)
	}
	if toolDef["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("tools[0].input_schema = %v", toolDef["input_schema"])
	}

	// —— 响应映射 ——
	if resp.StopReason != StopToolUse {
		t.Fatalf("StopReason = %q", resp.StopReason)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 13 {
		t.Fatalf("Usage = %+v", resp.Usage)
	}
	if len(resp.Message.Blocks) != 2 {
		t.Fatalf("Blocks 数 = %d，期望 2", len(resp.Message.Blocks))
	}
	if resp.Message.Blocks[0].Text != "先看看" {
		t.Fatalf("文本块 = %+v", resp.Message.Blocks[0])
	}
	if resp.Message.Blocks[1].ToolCallID != "toolu_1" || string(resp.Message.Blocks[1].Args) != `{"path":"b.go"}` {
		t.Fatalf("工具块 = %+v", resp.Message.Blocks[1])
	}
}

func TestAnthropicMergesConsecutiveSameRoleMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := readJSONBody(t, r)
		messages := got["messages"].([]any)
		if len(messages) != 1 {
			t.Errorf("连续两条 user 消息应合并为 1 条，实际 %d", len(messages))
			return
		}
		blocks := messages[0].(map[string]any)["content"].([]any)
		if len(blocks) != 2 {
			t.Errorf("合并后应有 2 个 block，实际 %d", len(blocks))
			return
		}
		if blocks[0].(map[string]any)["type"] != "tool_result" || blocks[1].(map[string]any)["type"] != "text" {
			t.Errorf("block 顺序应为 tool_result → text：%v", blocks)
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	req := Request{Messages: []Message{
		ToolResultMessage(Block{Kind: BlockToolResult, ToolCallID: "t1", Result: "结果"}),
		TextMessage(RoleUser, "继续"),
	}}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

func TestAnthropicStreamTextAndToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readJSONBody(t, r)
		if body["stream"] != true {
			t.Errorf("流式请求应带 stream=true，实际 %v", body["stream"])
		}
		writeSSE(t, w,
			"event: message_start\n"+
				"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n",
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"，世界\"}}\n\n",
			"event: content_block_stop\n"+
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read_file\",\"input\":{}}}\n\n",
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n",
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"a.go\\\"}\"}}\n\n",
			"event: content_block_stop\n"+
				"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: message_delta\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":20}}\n\n",
			"event: message_stop\n"+
				"data: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{})
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
	// 关键：Anthropic 的工具块 index 是 1，归一后的序号必须从 0 开始，
	// 这样两家的事件序列对调用方完全一致。
	if got := chunks[0].ToolCallDelta; got != nil {
		t.Fatalf("第一个 chunk 不应是工具分片：%+v", got)
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "read_file" || calls[0].Args != `{"path":"a.go"}` {
		t.Fatalf("合并后的工具调用 = %+v", calls[0])
	}
	if n := countDone(chunks); n != 1 {
		t.Fatalf("Done 出现 %d 次，期望 1", n)
	}
	if n := countUsage(chunks); n != 1 {
		t.Fatalf("usage 出现 %d 次，期望 1", n)
	}
	last := finalChunk(chunks)
	if last.Usage == nil || last.Usage.PromptTokens != 10 || last.Usage.CompletionTokens != 20 {
		t.Fatalf("最终 usage = %+v", last.Usage)
	}
	if last.StopReason != StopToolUse {
		t.Fatalf("StopReason = %q", last.StopReason)
	}
}

func TestAnthropicToolOrdinalsAreDenseAcrossText(t *testing.T) {
	// 两个工具块，中间夹文本块：归一序号应分别为 0 / 1，不受 content block index 影响。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t0\",\"name\":\"a\",\"input\":{}}}\n\n",
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
			"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"b\",\"input\":{}}}\n\n",
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"k\\\":1}\"}}\n\n",
			"event: message_stop\n"+
				"data: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	calls := mergeToolCalls(chunks)
	if len(calls) != 2 {
		t.Fatalf("工具调用数 = %d，期望 2", len(calls))
	}
	if calls[0].ID != "t0" || calls[0].Args != "{}" {
		t.Fatalf("calls[0] = %+v", calls[0])
	}
	if calls[1].ID != "t1" || calls[1].Args != `{"k":1}` {
		t.Fatalf("calls[1] = %+v", calls[1])
	}
}

func TestAnthropicErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"鉴权", http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"bad key"}}`, ErrAuth},
		{"上下文超限", http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"prompt is too long: 200000 tokens > 100000"}}`, ErrContextOverflow},
		{"限流", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow"}}`, ErrRateLimited},
		{"过载", http.StatusServiceUnavailable, `{"error":{"type":"overloaded_error","message":"busy"}}`, ErrTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
			_, err := a.Chat(context.Background(), Request{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.want)
			}
		})
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"event: error\n"+
				"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"服务过载\"}}\n\n",
		)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	_, err := collectStream(t, a, Request{})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("流内 error 事件应归类为 ErrTransient，实际 %v", err)
	}
}

func TestAnthropicEndpointAvoidsDoubleV1(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com":    "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/":   "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/v1": "https://api.anthropic.com/v1/messages",
	}
	for base, want := range cases {
		if got := newAnthropic("t", base, "", "m", http.DefaultClient).endpoint(); got != want {
			t.Fatalf("%s → %s，期望 %s", base, got, want)
		}
	}
}

func TestAnthropicEmptyToolResultStillSendsContent(t *testing.T) {
	// 空结果的 tool_result 也必须带 content 字段（Anthropic 视为必填）。
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readJSONBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	if _, err := a.Chat(context.Background(), Request{Messages: []Message{
		ToolResultMessage(Block{Kind: BlockToolResult, ToolCallID: "t1", Result: ""}),
	}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	block := got["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	content, ok := block["content"]
	if !ok {
		t.Fatalf("tool_result 必须带 content 字段：%v", block)
	}
	if content != "" {
		t.Fatalf("content = %v，期望空串", content)
	}
}

func TestAnthropicPutsToolResultBeforeText(t *testing.T) {
	// 同一条消息里 block 顺序可能被调用方写成 text → tool_result，
	// 协议要求 tool_result 打头，映射时必须重排。
	got := anthropicMessages([]Message{{Role: RoleUser, Blocks: []Block{
		{Kind: BlockText, Text: "结果如下"},
		{Kind: BlockToolResult, ToolCallID: "t1", Result: "ok"},
	}}})
	if len(got) != 1 || len(got[0].Content) != 2 {
		t.Fatalf("映射结果 = %+v", got)
	}
	if got[0].Content[0].Type != "tool_result" || got[0].Content[1].Type != "text" {
		t.Fatalf("block 顺序应为 tool_result → text：%+v", got[0].Content)
	}
}

func TestAnthropicStreamUsesFullInputFromContentBlockStart(t *testing.T) {
	// 有的代理只在 content_block_start 给完整 input、不发 input_json_delta。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"event: content_block_start\n"+
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"read_file\",\"input\":{\"path\":\"a.go\"}}}\n\n",
			"event: content_block_stop\n"+
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_stop\n"+
				"data: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	calls := mergeToolCalls(chunks)
	if len(calls) != 1 {
		t.Fatalf("工具调用数 = %d，期望 1", len(calls))
	}
	if calls[0].Args != `{"path":"a.go"}` {
		t.Fatalf("参数 = %q，期望从 content_block_start.input 取得", calls[0].Args)
	}
}

func TestAnthropicStreamDefaultsStopReasonWhenMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			"event: content_block_delta\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"完\"}}\n\n",
			"event: message_stop\n"+
				"data: {\"type\":\"message_stop\"}\n\n",
		)
	}))
	defer srv.Close()

	a := newAnthropic("test", srv.URL, "k", "m", srv.Client())
	chunks, err := collectStream(t, a, Request{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := finalChunk(chunks).StopReason; got != StopEnd {
		t.Fatalf("StopReason = %q，期望 %q", got, StopEnd)
	}
}

func TestAnthropicMergesConsecutiveToolResultsInOrder(t *testing.T) {
	// 调用方常为每个工具结果各追加一条 user 消息。合并后顺序必须与工具调用
	// 一致，不能因为“tool_result 打头”把后到的结果插到前面。
	got := anthropicMessages([]Message{
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolUse, ToolCallID: "t1", ToolName: "read"},
			{Kind: BlockToolUse, ToolCallID: "t2", ToolName: "grep"},
		}},
		ToolResultMessage(Block{Kind: BlockToolResult, ToolCallID: "t1", Result: "A"}),
		ToolResultMessage(Block{Kind: BlockToolResult, ToolCallID: "t2", Result: "B"}),
	})
	if len(got) != 2 {
		t.Fatalf("消息数 = %d，期望 2（user/assistant 交替）：%+v", len(got), got)
	}
	user := got[1].Content
	if len(user) != 2 {
		t.Fatalf("user 消息 block 数 = %d，期望 2", len(user))
	}
	if user[0].ToolUseID != "t1" || user[1].ToolUseID != "t2" {
		t.Fatalf("tool_result 顺序 = %s, %s，期望 t1, t2", user[0].ToolUseID, user[1].ToolUseID)
	}
}

func TestAnthropicStartArgsTreatsWhitespaceEmptyObjectAsEmpty(t *testing.T) {
	// 部分网关把 {} 序列化成 "{ }"，不能当成“已给完整参数”。
	for _, raw := range []string{`{}`, `{ }`, "{\n}", `null`, `  `} {
		if got := anthStartArgs(json.RawMessage(raw)); got != "" {
			t.Errorf("input=%q 应视为空参数，实际 %q", raw, got)
		}
	}
	got := anthStartArgs(json.RawMessage(`  {"path": "a.go"}  `))
	if got != `{"path":"a.go"}` {
		t.Errorf("完整参数应压掉空白，实际 %q", got)
	}
}
