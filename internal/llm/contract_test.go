package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// contractCase 是一张跨供应商行为表：同一份期望结果，两家都必须给出。
// 新适配器只要接上这张表，就能自证与既有实现一致（issue #2 的验收要求）。
type contractCase struct {
	name          string
	req           Request
	openaiBody    string
	anthropicBody string
	wantText      string
	wantCalls     []mergedCall
	wantUsage     Usage
	wantStop      StopReason
}

func TestCrossProviderStreamContract(t *testing.T) {
	cases := []contractCase{
		{
			name: "纯文本",
			req:  Request{Messages: []Message{TextMessage(RoleUser, "你好")}},
			openaiBody: `data: {"choices":[{"delta":{"content":"abc"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"def"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}

data: [DONE]

`,
			anthropicBody: `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"abc"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"def"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantText:  "abcdef",
			wantUsage: Usage{PromptTokens: 3, CompletionTokens: 4},
			wantStop:  StopEnd,
		},
		{
			name: "单工具调用",
			req:  Request{Messages: []Message{TextMessage(RoleUser, "读文件")}},
			openaiBody: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":8}}

data: [DONE]

`,
			anthropicBody: `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read_file","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantCalls: []mergedCall{{ID: "call_1", Name: "read_file", Args: `{"path":"a.go"}`}},
			wantUsage: Usage{PromptTokens: 7, CompletionTokens: 8},
			wantStop:  StopToolUse,
		},
		{
			name: "文本加工具调用",
			req:  Request{Messages: []Message{TextMessage(RoleUser, "看看")}},
			openaiBody: `data: {"choices":[{"delta":{"content":"先看一下"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"**/*.go\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":5}}

data: [DONE]

`,
			anthropicBody: `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":2,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"先看一下"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_9","name":"glob","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":\""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"**/*.go\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantText:  "先看一下",
			wantCalls: []mergedCall{{ID: "call_9", Name: "glob", Args: `{"pattern":"**/*.go"}`}},
			wantUsage: Usage{PromptTokens: 2, CompletionTokens: 5},
			wantStop:  StopToolUse,
		},
		{
			name: "usage 缺失",
			req:  Request{Messages: []Message{TextMessage(RoleUser, "hi")}},
			openaiBody: `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: [DONE]

`,
			anthropicBody: `event: message_start
data: {"type":"message_start","message":{}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantText:  "hi",
			wantUsage: Usage{},
			wantStop:  StopEnd,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("openai", func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					io.WriteString(w, tc.openaiBody)
				}))
				defer srv.Close()
				checkContract(t, tc, newOpenAI("openai", srv.URL, "k", "m", srv.Client()))
			})
			t.Run("anthropic", func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					io.WriteString(w, tc.anthropicBody)
				}))
				defer srv.Close()
				checkContract(t, tc, newAnthropic("anthropic", srv.URL, "k", "m", srv.Client()))
			})
		})
	}
}

func checkContract(t *testing.T, tc contractCase, p Streamer) {
	t.Helper()
	chunks, err := collectStream(t, p, tc.req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got := joinedText(chunks); got != tc.wantText {
		t.Errorf("文本 = %q，期望 %q", got, tc.wantText)
	}
	gotCalls := mergeToolCalls(chunks)
	if len(gotCalls) == 0 && len(tc.wantCalls) == 0 {
		// 两边都空，视为一致。
	} else if !reflect.DeepEqual(gotCalls, tc.wantCalls) {
		t.Errorf("工具调用 = %+v，期望 %+v", gotCalls, tc.wantCalls)
	}
	if n := countDone(chunks); n != 1 {
		t.Errorf("Done 出现 %d 次，期望 1", n)
	}
	if n := countUsage(chunks); n != 1 {
		t.Errorf("usage 出现 %d 次，期望 1", n)
	}
	last := finalChunk(chunks)
	if last.Usage == nil || *last.Usage != tc.wantUsage {
		t.Errorf("usage = %+v，期望 %+v", last.Usage, tc.wantUsage)
	}
	if last.StopReason != tc.wantStop {
		t.Errorf("停止原因 = %q，期望 %q", last.StopReason, tc.wantStop)
	}
}

// TestCrossProviderErrorContract 断言两家对同类 HTTP 失败给出同一个归一错误。
func TestCrossProviderErrorContract(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		openaiBody    string
		anthropicBody string
		want          error
	}{
		{
			name:          "限流",
			status:        http.StatusTooManyRequests,
			openaiBody:    `{"error":{"message":"rate limited"}}`,
			anthropicBody: `{"error":{"type":"rate_limit_error","message":"rate limited"}}`,
			want:          ErrRateLimited,
		},
		{
			name:          "鉴权失败",
			status:        http.StatusUnauthorized,
			openaiBody:    `{"error":{"message":"invalid api key"}}`,
			anthropicBody: `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			want:          ErrAuth,
		},
		{
			name:          "上下文超限",
			status:        http.StatusBadRequest,
			openaiBody:    `{"error":{"code":"context_length_exceeded","message":"too long"}}`,
			anthropicBody: `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`,
			want:          ErrContextOverflow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serve := func(body string) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					io.WriteString(w, body)
				}))
			}

			openaiSrv := serve(tc.openaiBody)
			defer openaiSrv.Close()
			if _, err := newOpenAI("openai", openaiSrv.URL, "k", "m", openaiSrv.Client()).Chat(context.Background(), Request{}); !errors.Is(err, tc.want) {
				t.Errorf("openai 错误 = %v，期望 %v", err, tc.want)
			}

			anthropicSrv := serve(tc.anthropicBody)
			defer anthropicSrv.Close()
			if _, err := newAnthropic("anthropic", anthropicSrv.URL, "k", "m", anthropicSrv.Client()).Chat(context.Background(), Request{}); !errors.Is(err, tc.want) {
				t.Errorf("anthropic 错误 = %v，期望 %v", err, tc.want)
			}
		})
	}
}
