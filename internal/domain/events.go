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
	// EventSessionUpdated 记录会话切换了供应商或模型。后续任务按新值构造
	// agent；已完成任务的历史不受影响。
	EventSessionUpdated EventKind = "session.updated"

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
	// EventProposalRejected 覆盖「提案没落盘」的全部结局：被拒、失效、写失败。
	// 结局写在载荷的 status 里而不是拆成三种事件类型：它们对回放消费者的
	// 含义是同一个——这次写入没有发生。
	EventProposalRejected EventKind = "proposal.rejected"
	EventToolResult       EventKind = "tool.result"
	EventError            EventKind = "error"

	// EventContextCompacted 记录一次上下文压缩：Summary 替换掉它之前的
	// CoveredMessages 条消息。它只影响上下文重建，不改变任务状态。
	EventContextCompacted EventKind = "context.compacted"
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
	Workspace string        `json:"workspace"`
	Roots     []SessionRoot `json:"roots,omitempty"`
	Provider  string        `json:"provider,omitempty"`
	Model     string        `json:"model,omitempty"`
	Title     string        `json:"title,omitempty"`
	// SharedWorkspace 列出同一工作区里还有 queued/running 任务的其它会话。
	// 只做提示：两个会话各改各的文件没问题，命令与构建会互踩。
	SharedWorkspace []string `json:"shared_workspace,omitempty"`
}

// SessionUpdatedPayload 记录会话切换后的供应商与模型。
type SessionUpdatedPayload struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
}

type TaskQueuedPayload struct {
	Prompt string `json:"prompt"`
	// Skills 是用户显式点名的技能名；重启续跑与回放据此重新加载正文。
	Skills []string `json:"skills,omitempty"`
	// Provider/Model 是这次任务的单次模型覆盖（空表示跟随会话）。
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
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

// ContextCompactedPayload 是一次上下文压缩的结果。
//
// Summary 会替换掉本事件之前的 CoveredMessages 条消息；token 数是压缩调用
// 自身的开销，计入任务总用量（它确实是花掉的钱）。
type ContextCompactedPayload struct {
	Summary          string `json:"summary"`
	CoveredMessages  int    `json:"covered_messages"`
	BeforeChars      int    `json:"before_chars"`
	SummaryChars     int    `json:"summary_chars"`
	Model            string `json:"model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
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
	// Truncated 表示 Diff 已被 patch.MaxDiffBytes 截断。客户端必须据此
	// 提示「你看到的不是全部」，否则就是在让人盲批一份没看全的改动。
	Truncated bool     `json:"truncated,omitempty"`
	Command   []string `json:"command,omitempty"`
	// Shell 是模型请求 shell 解释时的整条命令；非空时客户端应把它当作
	// 主体展示，不要再拆成 argv 三段。
	Shell string `json:"shell,omitempty"`
	// 命令审批必须展示真实 argv、cwd、后端与超时：用户批的应该是「实际要跑的
	// 东西」，而不是一句「要不要执行命令」。
	Cwd       string `json:"cwd,omitempty"`
	Backend   string `json:"backend,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// 以下字段只对 kind=external 有值：外部工具没有 diff / argv 可审，
	// 用户至少要看见「哪个来源的哪个工具、带了什么参数」。
	Tool   string `json:"tool,omitempty"`
	Source string `json:"source,omitempty"`
	Args   string `json:"args,omitempty"`
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
	// Hunks 只在部分应用时出现：写进文件的是这些块，其余块被丢弃。
	Hunks []int `json:"hunks,omitempty"`
	// TotalHunks 是原 diff 的总块数，配合 Hunks 说明「应用了几块里的哪几块」。
	TotalHunks int `json:"total_hunks,omitempty"`
}

// ProposalRejectedPayload 记录「这次写入没有发生」以及原因。
type ProposalRejectedPayload struct {
	ProposalID ProposalID     `json:"proposal_id"`
	Path       string         `json:"path"`
	Status     ProposalStatus `json:"status"`
	Reason     string         `json:"reason,omitempty"`
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
