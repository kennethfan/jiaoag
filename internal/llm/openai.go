package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// openaiAdapter 把 OpenAI 兼容协议（/chat/completions）翻译成归一类型。
//
// 它只做翻译：不重试、不限流、不落库。网络韧性由装饰器组合（ADR-0008）。
type openaiAdapter struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func newOpenAI(name, baseURL, apiKey, model string, client *http.Client) *openaiAdapter {
	return &openaiAdapter{name: name, baseURL: baseURL, apiKey: apiKey, model: model, client: client}
}

func (a *openaiAdapter) endpoint() string {
	return strings.TrimRight(a.baseURL, "/") + "/chat/completions"
}

// ---- 线格式（wire format） ----

type oaRequest struct {
	Model         string           `json:"model"`
	Messages      []oaMessage      `json:"messages"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	Stop          []string         `json:"stop,omitempty"`
	Tools         []oaTool         `json:"tools,omitempty"`
	ToolChoice    string           `json:"tool_choice,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *oaStreamOptions `json:"stream_options,omitempty"`
}

type oaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaMessage struct {
	Role string `json:"role"`
	// Content 用指针区分「不发送」与「显式空串」：tool 消息的 content 是
	// 必填字段，工具结果为空时也必须发出去（见 ptrString）。
	Content    *string      `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function oaFunctionCall `json:"function"`
}

type oaFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string       `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *oaError `json:"error"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string            `json:"content"`
			ToolCalls []oaToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *oaError `json:"error"`
}

// oaToolCallDelta 是流式工具调用分片：首个分片带 id/name，后续只带 arguments。
type oaToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ---- 请求映射 ----

func (a *openaiAdapter) buildRequest(req Request, stream bool) oaRequest {
	out := oaRequest{
		Model:       a.pickModel(req.Model),
		Messages:    openAIMessages(req),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stop:        req.Stop,
		Stream:      stream,
	}
	if stream {
		out.StreamOptions = &oaStreamOptions{IncludeUsage: true}
	}
	// tools 为空时不发送该字段：部分兼容网关会拒绝空数组。
	if len(req.Tools) > 0 {
		out.Tools = make([]oaTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, oaTool{
				Type: "function",
				Function: oaFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  schemaOrEmptyObject(t.Schema),
				},
			})
		}
		out.ToolChoice = "auto"
	}
	return out
}

func (a *openaiAdapter) pickModel(model string) string {
	if model != "" {
		return model
	}
	return a.model
}

// openAIMessages 把归一消息摊平成 OpenAI 的消息数组。
//
// 两处形状差异：
//   - assistant 的工具调用放在 tool_calls 字段，参数是字符串化的 JSON；
//   - 工具结果要拆成独立的 role=tool 消息，紧跟发起调用的 assistant 消息。
func openAIMessages(req Request) []oaMessage {
	out := make([]oaMessage, 0, len(req.Messages)+2)
	if req.System != "" {
		out = append(out, oaMessage{Role: "system", Content: ptrString(req.System)})
	}
	for _, m := range req.Messages {
		var (
			textParts []string
			calls     []oaToolCall
			results   []oaMessage
		)
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockText:
				if b.Text != "" {
					textParts = append(textParts, b.Text)
				}
			case BlockToolUse:
				calls = append(calls, oaToolCall{
					ID:       b.ToolCallID,
					Type:     "function",
					Function: oaFunctionCall{Name: b.ToolName, Arguments: rawOrEmptyObject(b.Args)},
				})
			case BlockToolResult:
				results = append(results, oaMessage{
					Role:       "tool",
					ToolCallID: b.ToolCallID,
					// 空结果也要显式发送 content:""，否则 OpenAI 会判为缺字段。
					Content: ptrString(b.Result),
				})
			}
		}
		// 工具结果必须先于后续用户文本进入消息数组。
		out = append(out, results...)
		text := strings.Join(textParts, "\n")
		if text != "" || len(calls) > 0 {
			msg := oaMessage{Role: string(m.Role), ToolCalls: calls}
			if text != "" {
				msg.Content = ptrString(text)
			}
			out = append(out, msg)
		}
	}
	return out
}

// ---- 响应映射 ----

func (a *openaiAdapter) Chat(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(a.buildRequest(req, false))
	if err != nil {
		return Response{}, fmt.Errorf("llm: 编码 openai 请求失败：%w", err)
	}
	httpReq, err := newJSONRequest(ctx, a.endpoint(), body, map[string]string{
		"Authorization": "Bearer " + a.apiKey,
	})
	if err != nil {
		return Response{}, err
	}
	resp, err := doJSON(ctx, a.client, a.name, httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var payload oaResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Response{}, fmt.Errorf("llm: 解析 openai 响应失败：%w: %w", ErrTransient, err)
	}
	if payload.Error != nil {
		return Response{}, fmt.Errorf("llm: openai 返回错误：%s: %w", payload.Error.Message, ErrTransient)
	}
	if len(payload.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: openai 响应不含 choices：%w", ErrTransient)
	}

	choice := payload.Choices[0]
	var blocks []Block
	if choice.Message.Content != "" {
		blocks = append(blocks, Block{Kind: BlockText, Text: choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		blocks = append(blocks, Block{
			Kind:       BlockToolUse,
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Args:       compactJSON(json.RawMessage(tc.Function.Arguments)),
		})
	}

	out := Response{
		Message:    Message{Role: RoleAssistant, Blocks: blocks},
		StopReason: openAIStopReason(choice.FinishReason),
		Model:      payload.Model,
	}
	if payload.Usage != nil {
		out.Usage = Usage{PromptTokens: payload.Usage.PromptTokens, CompletionTokens: payload.Usage.CompletionTokens}
	}
	return out, nil
}

func (a *openaiAdapter) ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error {
	body, err := json.Marshal(a.buildRequest(req, true))
	if err != nil {
		return fmt.Errorf("llm: 编码 openai 流式请求失败：%w", err)
	}
	httpReq, err := newJSONRequest(ctx, a.endpoint(), body, map[string]string{
		"Authorization": "Bearer " + a.apiKey,
		"Accept":        "text/event-stream",
	})
	if err != nil {
		return err
	}
	resp, err := doJSON(ctx, a.client, a.name, httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var (
		usage *Usage
		// stop 默认 StopEnd：与非流式的 openAIStopReason("") 一致。代价是
		// 「上游没给 finish_reason 就正常关闭」与「回答讲完了」不可区分，
		// 需要截断检测时由上层结合工具调用闭环判断。
		stop     = StopEnd
		ordinals = make(map[int]int) // 线格式 index → 工具调用序号（保证两家一致）
		nextOrd  int
	)

	err = readSSE(a.name, resp.Body, func(_ string, data []byte) error {
		payload := strings.TrimSpace(string(data))
		if payload == "" || payload == "[DONE]" {
			return nil
		}

		var chunk oaStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("llm: 解析 openai 流式数据失败：%w: %w", ErrTransient, err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("llm: openai 流式返回错误：%s: %w", chunk.Error.Message, ErrTransient)
		}
		if chunk.Usage != nil {
			usage = &Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := fn(Chunk{TextDelta: choice.Delta.Content}); err != nil {
					return err
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments == "" {
					continue
				}
				ord, ok := ordinals[tc.Index]
				if !ok {
					ord = nextOrd
					ordinals[tc.Index] = ord
					nextOrd++
				}
				delta := ToolCallDelta{Index: ord, ID: tc.ID, Name: tc.Function.Name, ArgsFragment: tc.Function.Arguments}
				if err := fn(Chunk{ToolCallDelta: &delta}); err != nil {
					return err
				}
			}
			if choice.FinishReason != "" {
				stop = openAIStopReason(choice.FinishReason)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Done 恰好一次，并携带最终 usage 与停止原因。
	// 即使上游没报 usage，也给零值指针：两家在这一点的行为必须一致。
	final := Usage{}
	if usage != nil {
		final = *usage
	}
	return fn(Chunk{Usage: &final, StopReason: stop, Done: true})
}

func openAIStopReason(reason string) StopReason {
	switch reason {
	case "", "stop":
		return StopEnd
	case "length":
		return StopLength
	case "tool_calls", "function_call":
		return StopToolUse
	default:
		return StopOther
	}
}

// rawOrEmptyObject 保证工具参数是合法 JSON 对象，避免发出空串。
func rawOrEmptyObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// schemaOrEmptyObject 同 rawOrEmptyObject，用于工具 schema。
func schemaOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
