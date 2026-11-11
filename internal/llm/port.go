// Package llm 是模型供应商端口：只定义归一后的请求、响应与流式增量。
//
// 具体协议由薄适配器翻译（openai / anthropic），重试与超时由装饰器组合
// （ADR-0008）。本包只依赖标准库，不 import internal/{store,app,server}，
// 也不落库——流式增量不入库是 ADR-0005 的既有决定。
package llm

import (
	"context"
	"encoding/json"
)

// Role 是归一后的消息角色。system 提示单独放在 Request.System，不占角色。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockKind 是消息块类型。
type BlockKind string

const (
	BlockText       BlockKind = "text"        // 普通文本
	BlockToolUse    BlockKind = "tool_use"    // 模型请求调用工具
	BlockToolResult BlockKind = "tool_result" // 调用方回填的工具结果
)

// Block 是消息里的一个块。
//
// 用扁平结构而不是接口：两家协议的 block 形状差别大，但真正要表达的就这
// 三种，扁平结构让映射代码与 JSON 编解码都更直接。
type Block struct {
	Kind BlockKind

	// Text 用于 BlockText。
	Text string

	// 以下字段用于工具块。BlockToolUse 用 ToolCallID/ToolName/Args，
	// BlockToolResult 用 ToolCallID/Result/IsError。
	ToolCallID string
	ToolName   string
	Args       json.RawMessage
	Result     string
	IsError    bool
}

// Message 是归一后的一条消息。
type Message struct {
	Role   Role
	Blocks []Block
}

// TextMessage 构造一条纯文本消息。
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Blocks: []Block{{Kind: BlockText, Text: text}}}
}

// ToolResultMessage 构造一条只含工具结果的消息（两家都要求它是 user 侧消息）。
func ToolResultMessage(results ...Block) Message {
	return Message{Role: RoleUser, Blocks: results}
}

// ToolSpec 是暴露给模型的工具定义。
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema
}

// Request 是一次模型调用的归一入参。
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature *float64
	Stop        []string
}

// Usage 是 token 统计，两家字段名不同，这里归一。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// StopReason 是归一后的停止原因。
type StopReason string

const (
	StopEnd     StopReason = "end"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "tool_use"
	StopOther   StopReason = "other"
)

// Response 是非流式调用的结果。
type Response struct {
	Message    Message
	Usage      Usage
	StopReason StopReason
	Model      string
}

// ToolCallDelta 是流式工具调用的一个分片。
//
// 首个分片带 ID/Name，后续分片只带 ArgsFragment，调用方按 Index 拼接。
type ToolCallDelta struct {
	Index        int
	ID           string
	Name         string
	ArgsFragment string
}

// Chunk 是流式增量。Done 在正常结束时恰好出现一次，并携带最终 usage。
type Chunk struct {
	TextDelta     string
	ToolCallDelta *ToolCallDelta
	Usage         *Usage
	StopReason    StopReason
	Done          bool
}

// Provider 是非流式基线：一次请求换一个完整响应。
type Provider interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

// Streamer 是可选增强：逐块回调。回调返回 error 表示调用方要求中止。
type Streamer interface {
	ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error
}
