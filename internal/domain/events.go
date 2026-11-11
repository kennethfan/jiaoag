package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventVersion 是事件信封的格式版本。载荷本身不做版本化：加字段向后兼容，
// 破坏性变更必须新增事件类型（ADR-0005）。
const EventVersion = 1

// EventKind 是事件类型。它是日志的稳定标识，只在万不得已时新增。
type EventKind string

const (
	EventSessionCreated EventKind = "session.created"

	// 任务状态统一走 task.status{from,to,reason}，不按状态拆成多种类型。
	EventTaskQueued EventKind = "task.queued"
	EventTaskStatus EventKind = "task.status"

	EventRoundStarted EventKind = "round.started"

	// 流式增量（assistant.delta）不落库，只持久化边界事件。
	EventAssistantDelta    EventKind = "assistant.delta"
	EventAssistantStart    EventKind = "assistant.start"
	EventAssistantMessage  EventKind = "assistant.message"
	EventToolRequested     EventKind = "tool.requested"
	EventToolPrepared      EventKind = "tool.prepared"
	EventApprovalRequested EventKind = "approval.requested"
	EventApprovalResolved  EventKind = "approval.resolved"
	EventProposalCreated   EventKind = "proposal.created"
	EventProposalApplied   EventKind = "proposal.applied"
	EventToolResult        EventKind = "tool.result"
	EventError             EventKind = "error"
)

// Event 是事件日志里的一个不可变事实。
//
// Seq 由存储层在追加事务内分配；此处的零值表示「尚未分配」。
type Event struct {
	V         int             `json:"v"`
	Type      EventKind       `json:"type"`
	SessionID SessionID       `json:"session_id"`
	TaskID    TaskID          `json:"task_id,omitempty"`
	Seq       Seq             `json:"seq"`
	At        time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// NewEvent 构造一个待追加的事件；Seq 与时间由存储层补齐。
func NewEvent(kind EventKind, sessionID SessionID, taskID TaskID, payload any) (Event, error) {
	ev := Event{V: EventVersion, Type: kind, SessionID: sessionID, TaskID: taskID}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("domain: marshal %s payload: %w", kind, err)
		}
		ev.Payload = raw
	}
	return ev, nil
}

// Validate 检查事件的基本完整性（不检查 Seq，因为追加前它还是零值）。
func (e Event) Validate() error {
	if e.Type == "" {
		return fmt.Errorf("domain: event type is empty")
	}
	if !e.SessionID.Valid() {
		return fmt.Errorf("domain: event %s has no session", e.Type)
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return fmt.Errorf("domain: event %s payload is not valid JSON", e.Type)
	}
	return nil
}

// Decode 把载荷解析到 v。旧事件缺少新字段时留零值，不报错。
func (e Event) Decode(v any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("domain: event %s has no payload", e.Type)
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("domain: decode %s payload: %w", e.Type, err)
	}
	return nil
}

// 以下载荷结构按事件类型划分。M1 只发 session.created / task.queued /
// task.status，其余为后续里程碑预置（字段先落，避免日志格式反复变动）。

type SessionCreatedPayload struct {
	Workspace string `json:"workspace"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Title     string `json:"title,omitempty"`
}

type TaskQueuedPayload struct {
	Prompt           string `json:"prompt"`
	SystemPromptHash string `json:"system_prompt_hash,omitempty"`
	ExtensionsDigest string `json:"extensions_digest,omitempty"`
}

type TaskStatusPayload struct {
	From      TaskStatus `json:"from"`
	To        TaskStatus `json:"to"`
	Reason    string     `json:"reason,omitempty"`
	Error     string     `json:"error,omitempty"`
	Round     int        `json:"round,omitempty"`
	PromptTok int        `json:"prompt_tokens,omitempty"`
	OutputTok int        `json:"completion_tokens,omitempty"`
}

type RoundStartedPayload struct {
	Round int `json:"round"`
}

type AssistantStartPayload struct {
	Round int    `json:"round"`
	Model string `json:"model,omitempty"`
}

// AssistantDeltaPayload 是流式文本增量。它只出现在临时帧里（Seq 为 0），
// 不写事件日志（ADR-0005）：客户端按到达顺序渲染，重连后以完整消息为准。
type AssistantDeltaPayload struct {
	Text string `json:"text"`
}

type AssistantMessagePayload struct {
	Round int    `json:"round"`
	Text  string `json:"text"`

	// ToolCalls 保留模型这一轮请求的工具调用。重建上下文需要 ID/name/args，
	// 只存 Text 的话，重连后历史里就只剩助手说过的话、丢了它要做什么。
	ToolCalls  []AssistantToolCallPayload `json:"tool_calls,omitempty"`
	StopReason string                     `json:"stop_reason,omitempty"`
}

// AssistantToolCallPayload 是一条完整的工具调用（流式分片已拼好）。
type AssistantToolCallPayload struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type ToolRequestedPayload struct {
	Tool       string          `json:"tool"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
}

type ToolPreparedPayload struct {
	Tool       string   `json:"tool"`
	SideEffect string   `json:"side_effect,omitempty"`
	Risk       Risk     `json:"risk,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	Command    []string `json:"command,omitempty"`
}

type ApprovalRequestedPayload struct {
	ApprovalID ApprovalID   `json:"approval_id"`
	Kind       ApprovalKind `json:"kind"`
	Risk       Risk         `json:"risk,omitempty"`
	Path       string       `json:"path,omitempty"`
	Diff       string       `json:"diff,omitempty"`
	Command    []string     `json:"command,omitempty"`
}

type ApprovalResolvedPayload struct {
	ApprovalID ApprovalID     `json:"approval_id"`
	Decision   Decision       `json:"decision"`
	Source     ApprovalSource `json:"source"`
	Note       string         `json:"note,omitempty"`
}

type ProposalCreatedPayload struct {
	ProposalID ProposalID `json:"proposal_id"`
	Path       string     `json:"path"`
	Diff       string     `json:"diff,omitempty"`
	BaseHash   string     `json:"base_hash"`
}

type ProposalAppliedPayload struct {
	ProposalID ProposalID `json:"proposal_id"`
	Path       string     `json:"path"`
	NewHash    string     `json:"new_hash"`
}

type ToolResultPayload struct {
	Tool       string `json:"tool"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Status     string `json:"status"`
	IsError    bool   `json:"is_error,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Output     string `json:"output,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type ErrorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}
