// Package domain 定义 jiaoag 的领域类型与状态机。
//
// 只依赖标准库：这里不出现数据库、网络、协议或供应商的细节。
// 所有其它包都依赖本包，反向依赖视为架构违规（见架构文档 §2）。
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// 标识符。全部是字符串别名，避免在函数签名里互相写错。
type (
	SessionID  string
	TaskID     string
	ToolCallID string
	ApprovalID string
	ProposalID string
)

// Seq 是会话内单调递增的事件序号。
// 允许空洞：流式增量不落库，因此不占号（ADR-0005）。
type Seq int64

func (id SessionID) String() string  { return string(id) }
func (id TaskID) String() string     { return string(id) }
func (id ToolCallID) String() string { return string(id) }
func (id ApprovalID) String() string { return string(id) }
func (id ProposalID) String() string { return string(id) }

// Valid 报告标识符是否非空。空标识一律视为编程错误。
func (id SessionID) Valid() bool  { return id != "" }
func (id TaskID) Valid() bool     { return id != "" }
func (id ToolCallID) Valid() bool { return id != "" }
func (id ApprovalID) Valid() bool { return id != "" }
func (id ProposalID) Valid() bool { return id != "" }

// Now 返回 UTC 时间。库里的时间一律由服务端生成，避免多客户端时钟漂移。
func Now() time.Time { return time.Now().UTC() }

// Session 是用户与某个工作区之间的一段连续工作。
type Session struct {
	ID        SessionID
	Workspace string
	Provider  string
	Model     string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Task 是一次用户请求，按轮次推进。状态迁移见 state.go。
type Task struct {
	ID               TaskID
	SessionID        SessionID
	Status           TaskStatus
	Prompt           string
	SystemPromptHash string
	ExtensionsDigest string
	LastSeq          Seq
	CreatedAt        time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
	Error            string
	PromptTokens     int
	CompleteTokens   int
}

// Round 是一次「模型调用 + 工具执行」的完整循环，不单独存表，
// 通过事件与 Task.Round 体现。
type Round int

type ToolCallStatus string

const (
	ToolRequested      ToolCallStatus = "requested"
	ToolPrepared       ToolCallStatus = "prepared"
	ToolWaitingApprove ToolCallStatus = "waiting_approval"
	ToolRunning        ToolCallStatus = "running"
	ToolSucceeded      ToolCallStatus = "succeeded"
	ToolFailed         ToolCallStatus = "failed"
	ToolDenied         ToolCallStatus = "denied"
	ToolCanceled       ToolCallStatus = "canceled"
)

// ToolCall 记录一次工具调用的输入、状态与输出引用。
type ToolCall struct {
	ID         ToolCallID
	TaskID     TaskID
	SessionID  SessionID
	Round      int
	Name       string
	Args       json.RawMessage
	Status     ToolCallStatus
	ApprovalID ApprovalID
	OutputPath string
	ExitCode   *int
	Error      string
	CreatedAt  time.Time
	EndedAt    time.Time
}

// ApprovalKind 区分审批的对象形态。
type ApprovalKind string

const (
	ApprovalDiff    ApprovalKind = "diff"
	ApprovalCommand ApprovalKind = "command"
)

// Decision 是审批结论；空值表示尚未裁决。
type Decision string

const (
	DecisionAllowOnce    Decision = "allow_once"
	DecisionAllowSession Decision = "allow_session"
	DecisionDeny         Decision = "deny"
	DecisionExpired      Decision = "expired"
)

// ApprovalSource 区分人工批准与策略放行，必须入审计。
type ApprovalSource string

const (
	SourceUser   ApprovalSource = "user"
	SourcePolicy ApprovalSource = "policy"
)

type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// Approval 是一次待裁决或已裁决的审批。
type Approval struct {
	ID          ApprovalID
	TaskID      TaskID
	ToolCallID  ToolCallID
	Kind        ApprovalKind
	Risk        Risk
	Decision    Decision
	Source      ApprovalSource
	RequestedAt time.Time
	ResolvedAt  time.Time
	Note        string
}

type ProposalStatus string

const (
	ProposalPending  ProposalStatus = "pending"
	ProposalApplied  ProposalStatus = "applied"
	ProposalRejected ProposalStatus = "rejected"
	ProposalStale    ProposalStatus = "stale"
)

// Proposal 是一次变更提案：批准前落库，批准后 CAS 落盘。
type Proposal struct {
	ID         ProposalID
	TaskID     TaskID
	ToolCallID ToolCallID
	Path       string
	Diff       string
	BaseHash   string
	NewHash    string
	Status     ProposalStatus
	AppliedAt  time.Time
}

// AuditRecord 只追加、不更新。它是事后追责的唯一依据。
type AuditRecord struct {
	ID         int64
	At         time.Time
	Actor      string
	Action     string
	ToolCallID ToolCallID
	Detail     json.RawMessage
}

// Transition 是一次原子的状态变更请求：投影与事件同事务提交。
//
// To 为空表示不改任务状态；ToolCall / Approval / Proposal 为空表示不触碰。
// Events 里的 Seq 与时间由存储层统一分配。
type Transition struct {
	TaskID   TaskID
	To       TaskStatus
	Reason   string
	ToolCall *ToolCall
	Approval *Approval
	Proposal *Proposal
	Audit    []AuditRecord
	Events   []Event
}

// Validate 做不涉及当前状态的检查；状态迁移合法性由存储层加载任务后判定。
func (t Transition) Validate() error {
	if !t.TaskID.Valid() {
		return fmt.Errorf("transition: task id is empty")
	}
	if t.To != "" && !t.To.Valid() {
		return fmt.Errorf("transition: unknown task status %q", t.To)
	}
	for i, ev := range t.Events {
		if err := ev.Validate(); err != nil {
			return fmt.Errorf("transition: events[%d]: %w", i, err)
		}
	}
	return nil
}
