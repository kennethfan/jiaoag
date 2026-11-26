// Package domain 定义 jiaoag 的领域类型与状态机。
//
// 只依赖标准库：这里不出现数据库、网络、协议或供应商的细节。
// 所有其它包都依赖本包，反向依赖视为架构违规（见架构文档 §2）。
package domain

import (
	"crypto/rand"
	"encoding/hex"
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

// NewID 生成带前缀的随机标识，例如 ses_1f3c…。
//
// 放在 domain 而不是 app：agent 要自己的审批/提案标识，但它不依赖应用层。
func NewID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("domain: 生成标识: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(buf[:]), nil
}

// Session 是用户与某个工作区之间的一段连续工作。
type Session struct {
	ID        SessionID
	Workspace string
	// Roots 是只读附加根（ADR-0036）：alias 用于 alias:path 前缀。主根不在其中。
	Roots     []SessionRoot
	Provider  string
	Model     string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SessionRoot 是会话挂载的只读附加根。
type SessionRoot struct {
	Alias string `json:"alias"`
	Path  string `json:"path"`
}

// SessionSummary 是会话管理面板用的读模型：会话本体 + 计数。
//
// 计数放在读模型里而不是 Session 上：会话本身的字段是任务构造的输入，
// 统计只服务展示，混在一起会让「会话」这个概念变得越来越沉。
type SessionSummary struct {
	Session
	Tasks            int
	ActiveTasks      int
	PromptTokens     int
	CompletionTokens int
}

// Task 是一次用户请求，按轮次推进。状态迁移见 state.go。
type Task struct {
	ID        TaskID
	SessionID SessionID
	Status    TaskStatus
	Prompt    string
	// Skills 是提交时用户显式点名的技能名（由 /skill 面板选择）。
	Skills []string
	// Provider/Model 是这次任务实际使用的供应商与模型。空表示跟随会话默认；
	// 非空来自提交时的单次覆盖（C1），重启续跑按它重建，而不是回落到会话设置。
	Provider         string
	Model            string
	SystemPromptHash string
	ExtensionsDigest string
	LastSeq          Seq
	// Owner 是占用这条任务的运行时 id（见 store.ClaimTask）。空表示还没有人占位。
	Owner          string
	CreatedAt      time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	Error          string
	PromptTokens   int
	CompleteTokens int
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

// Terminal 报告工具调用是否已经结束（不再接受迁移）。
func (s ToolCallStatus) Terminal() bool {
	switch s {
	case ToolSucceeded, ToolFailed, ToolDenied, ToolCanceled:
		return true
	}
	return false
}

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
	// ApprovalExternal 是外部工具（MCP / 插件）的调用审批：既没有 diff 也
	// 没有 argv，宿主只能展示工具名、来源与参数摘要。
	ApprovalExternal ApprovalKind = "external"
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
	// SourceSystem 表示不是人也不是策略的决定，例如进程重启后清理遗留的
	// 待审批项（标成 expired）。审计里必须能和人工裁决区分开。
	SourceSystem ApprovalSource = "system"
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
	// ProposalFailed 表示批准了但没能落盘（磁盘满、权限不足等）。
	// 它与 stale 分开：stale 是「文件被别人改了」，failed 是「写不进去」，
	// 用户看到这两句时的下一步动作完全不同。
	ProposalFailed ProposalStatus = "failed"
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
	// TaskID / SessionID 由读取侧（store.ListAudit）通过 tool_calls 关联填充，
	// 写入时忽略：audit 表本身没有这两列。
	TaskID    TaskID
	SessionID SessionID
}

// TokenUsage 是一次模型调用的 token 统计。agent 每轮累加，随状态迁移一起
// 提交，避免为计数单独发一条事件。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
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
	Usage    *TokenUsage
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
