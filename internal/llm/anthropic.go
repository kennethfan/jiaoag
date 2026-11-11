package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicAdapter 把 Anthropic Messages API（/v1/messages）翻译成归一类型。
//
// 与 OpenAI 的核心差异（都在本文件内吸收）：
//   - system 是顶层字段，不占消息角色；
//   - 工具结果必须作为 user 消息里的 tool_result block；
//   - max_tokens 必填，配置缺省时用 defaultAnthropicMaxTokens；
//   - 流式是 content_block_* 事件，工具参数由 input_json_delta 分片拼接。
type anthropicAdapter struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// defaultAnthropicMaxTokens 是 Anthropic 必填 max_tokens 的兜底值。
const defaultAnthropicMaxTokens = 4096

// anthropicVersion 是 Messages API 的版本头，固定值。
const anthropicVersion = "2023-06-01"

func newAnthropic(name, baseURL, apiKey, model string, client *http.Client) *anthropicAdapter {
	return &anthropicAdapter{name: name, baseURL: baseURL, apiKey: apiKey, model: model, client: client}
}

func (a *anthropicAdapter) endpoint() string {
	base := strings.TrimRight(a.baseURL, "/")
	// 兼容 base_url 已经带 /v1 的写法，避免拼出 /v1/v1/messages。
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

// ---- 线格式 ----

type anthRequest struct {
	Model         string        `json:"model"`
	MaxTokens     int           `json:"max_tokens"`
	System        string        `json:"system,omitempty"`
	Messages      []anthMessage `json:"messages"`
	Tools         []anthTool    `json:"tools,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
}

type anthMessage struct {
	Role    string      `json:"role"`
	Content []anthBlock `json:"content"`
}

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	// Content 只用于 tool_result，且是必填：用指针区分「不发送」与「空串」。
	Content *string `json:"content,omitempty"`
	IsError bool    `json:"is_error,omitempty"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthResponse struct {
	Model      string      `json:"model"`
	Content    []anthBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthStreamEvent 覆盖 message_start / content_block_* / message_delta / message_stop。
type anthStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`

	ContentBlock *struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---- 请求映射 ----

func (a *anthropicAdapter) buildRequest(req Request, stream bool) anthRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}
	out := anthRequest{
		Model:         a.pickModel(req.Model),
		MaxTokens:     maxTokens,
		System:        req.System,
		Messages:      anthropicMessages(req.Messages),
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]anthTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, anthTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schemaOrEmptyObject(t.Schema),
			})
		}
	}
	return out
}

func (a *anthropicAdapter) pickModel(model string) string {
	if model != "" {
		return model
	}
	return a.model
}

// anthropicMessages 把归一消息摊平成 Anthropic 的 block 数组。
//
// 两条协议约束：
//   - 同一消息里 tool_result 必须排在其它 block 之前；
//   - user/assistant 必须交替，而「工具结果 + 追加说明」天然产生两条连续的
//     user 消息，所以要合并。
func anthropicMessages(messages []Message) []anthMessage {
	out := make([]anthMessage, 0, len(messages))
	for _, m := range messages {
		var (
			results []anthBlock
			others  []anthBlock
		)
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockText:
				if b.Text != "" {
					others = append(others, anthBlock{Type: "text", Text: b.Text})
				}
			case BlockToolUse:
				others = append(others, anthBlock{
					Type:  "tool_use",
					ID:    b.ToolCallID,
					Name:  b.ToolName,
					Input: schemaOrEmptyObject(b.Args),
				})
			case BlockToolResult:
				results = append(results, anthBlock{
					Type:      "tool_result",
					ToolUseID: b.ToolCallID,
					Content:   ptrString(b.Result),
					IsError:   b.IsError,
				})
			}
		}
		if len(results)+len(others) == 0 {
			continue
		}
		role := "user"
		if m.Role == RoleAssistant {
			role = "assistant"
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			// 合并时 tool_result 仍要打头：否则前面那条消息的文本会把它挤到后面。
			// 但要插在「已有的 tool_result 之后」，不能整体前置——调用方常常为
			// 每个工具结果各追加一条消息，整体前置会把它们的顺序颠倒过来。
			prev := out[n-1].Content
			at := 0
			for at < len(prev) && prev[at].Type == "tool_result" {
				at++
			}
			merged := make([]anthBlock, 0, len(prev)+len(results)+len(others))
			merged = append(merged, prev[:at]...)
			merged = append(merged, results...)
			merged = append(merged, prev[at:]...)
			merged = append(merged, others...)
			out[n-1].Content = merged
			continue
		}
		blocks := make([]anthBlock, 0, len(results)+len(others))
		blocks = append(blocks, results...)
		blocks = append(blocks, others...)
		out = append(out, anthMessage{Role: role, Content: blocks})
	}
	return out
}

// ---- 响应映射 ----

func (a *anthropicAdapter) Chat(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(a.buildRequest(req, false))
	if err != nil {
		return Response{}, fmt.Errorf("llm: 编码 anthropic 请求失败：%w", err)
	}
	httpReq, err := newJSONRequest(ctx, a.endpoint(), body, a.headers())
	if err != nil {
		return Response{}, err
	}
	resp, err := doJSON(ctx, a.client, a.name, httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var payload anthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Response{}, fmt.Errorf("llm: 解析 anthropic 响应失败：%w: %w", ErrTransient, err)
	}
	if payload.Error != nil {
		return Response{}, fmt.Errorf("llm: anthropic 返回错误：%s: %w", payload.Error.Message, ErrTransient)
	}

	blocks := make([]Block, 0, len(payload.Content))
	for _, b := range payload.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				blocks = append(blocks, Block{Kind: BlockText, Text: b.Text})
			}
		case "tool_use":
			blocks = append(blocks, Block{
				Kind:       BlockToolUse,
				ToolCallID: b.ID,
				ToolName:   b.Name,
				Args:       compactJSON(b.Input),
			})
		}
	}
	return Response{
		Message:    Message{Role: RoleAssistant, Blocks: blocks},
		Usage:      Usage{PromptTokens: payload.Usage.InputTokens, CompletionTokens: payload.Usage.OutputTokens},
		StopReason: anthropicStopReason(payload.StopReason),
		Model:      payload.Model,
	}, nil
}

func (a *anthropicAdapter) ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error {
	body, err := json.Marshal(a.buildRequest(req, true))
	if err != nil {
		return fmt.Errorf("llm: 编码 anthropic 流式请求失败：%w", err)
	}
	httpReq, err := newJSONRequest(ctx, a.endpoint(), body, a.headers())
	if err != nil {
		return err
	}
	resp, err := doJSON(ctx, a.client, a.name, httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var (
		usage Usage
		// stop 默认 StopEnd：与非流式的 anthropicStopReason("") 一致。代价是
		// 「上游没给 stop_reason 就正常关闭」与「回答讲完了」不可区分，
		// 需要截断检测时由上层结合工具调用闭环判断。
		stop     = StopEnd
		ordinals = make(map[int]int) // content block index → 工具调用序号
		nextOrd  int
		done     bool
	)

	err = readSSE(a.name, resp.Body, func(event string, data []byte) error {
		var ev anthStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("llm: 解析 anthropic 流式数据失败：%w: %w", ErrTransient, err)
		}
		if ev.Type == "" {
			ev.Type = event
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.PromptTokens = ev.Message.Usage.InputTokens
				usage.CompletionTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				return nil
			}
			switch ev.ContentBlock.Type {
			case "text":
				if ev.ContentBlock.Text != "" {
					return fn(Chunk{TextDelta: ev.ContentBlock.Text})
				}
			case "tool_use":
				ord := nextOrd
				ordinals[ev.Index] = ord
				nextOrd++
				delta := ToolCallDelta{Index: ord, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
				// 官方实现发 input:{} 再靠 input_json_delta 拼；但有的代理只发
				// 完整 input 不发 delta，这时必须把参数原样带出去。
				delta.ArgsFragment = anthStartArgs(ev.ContentBlock.Input)
				return fn(Chunk{ToolCallDelta: &delta})
			}
		case "content_block_delta":
			if ev.Delta == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					return fn(Chunk{TextDelta: ev.Delta.Text})
				}
			case "input_json_delta":
				ord, ok := ordinals[ev.Index]
				if !ok {
					return fmt.Errorf("llm: anthropic 在未声明的工具块上传入参数（index=%d）：%w", ev.Index, ErrTransient)
				}
				if ev.Delta.PartialJSON != "" {
					delta := ToolCallDelta{Index: ord, ArgsFragment: ev.Delta.PartialJSON}
					return fn(Chunk{ToolCallDelta: &delta})
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = anthropicStopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				usage.CompletionTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			done = true
			u := usage
			return fn(Chunk{Usage: &u, StopReason: stop, Done: true})
		case "error":
			message := "未知错误"
			if ev.Error != nil && ev.Error.Message != "" {
				message = ev.Error.Message
			}
			return fmt.Errorf("llm: anthropic 流式返回错误：%s: %w", message, ErrTransient)
		case "ping":
			// 心跳，忽略。
		}
		return nil
	})
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	// 某些网关在 message_stop 之前就断开：仍要给调用方一个明确的结束信号。
	u := usage
	return fn(Chunk{Usage: &u, StopReason: stop, Done: true})
}

func (a *anthropicAdapter) headers() map[string]string {
	return map[string]string{
		"x-api-key":         a.apiKey,
		"anthropic-version": anthropicVersion,
		"Accept":            "text/event-stream",
	}
}

func anthropicStopReason(reason string) StopReason {
	switch reason {
	case "", "end_turn", "stop_sequence":
		return StopEnd
	case "max_tokens":
		return StopLength
	case "tool_use":
		return StopToolUse
	default:
		return StopOther
	}
}

// anthStartArgs 取出 content_block_start 里可能已经完整给出的工具参数。
//
// 空串、null 与空对象都表示「参数还在后续 delta 里」，不能参与拼接——否则会与
// 后续片段拼出非法 JSON。空对象要先压掉空白再比较：部分网关（如 Jackson 的
// pretty printer）会把 {} 序列化成 "{ }"，直接比字面量会漏判。
// 非空值说明代理一次性给了完整参数，压掉空白后作为首个片段。
func anthStartArgs(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(text)); err == nil {
		text = compact.String()
	}
	if text == "{}" {
		return ""
	}
	return text
}
