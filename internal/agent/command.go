package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kennethfan/jiaoag/internal/domain"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// 审计动作集合（issue #5）。一次执行恰写一条，且与 tool.result 同事务。
const (
	auditExecAllow    = "exec.allow"
	auditExecApproved = "exec.approved"
	auditExecDenied   = "exec.denied"
	auditExecTimeout  = "exec.timeout"
	auditExecFailed   = "exec.failed"
	auditToolDenied   = "tool.denied"
)

// execAuditDetail 是审计记录里的事实：事后要能回答「刚才那条命令到底跑了
// 什么、退出码多少、当时用的是哪套规则与哪个后端」。
type execAuditDetail struct {
	Argv           []string        `json:"argv"`
	Cwd            string          `json:"cwd"`
	Backend        string          `json:"backend,omitempty"`
	BackendVersion string          `json:"backend_version,omitempty"`
	PolicyVersion  string          `json:"policy_version,omitempty"`
	RuleID         string          `json:"rule_id,omitempty"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	OutputPath     string          `json:"output_path,omitempty"`
	LogError       string          `json:"log_error,omitempty"`
	DurationMS     int64           `json:"duration_ms,omitempty"`
	Limits         *execpkg.Limits `json:"limits,omitempty"`
	LimitsDegraded bool            `json:"limits_degraded,omitempty"`
}

// markCommandRunning 把命令调用投影成 running（不产生事件）。
//
// 与 recordPrepared 里「准备完不算 running」的约定配套：准备阶段停在
// prepared，只有审批/策略决定落定、进程真的要启动时才写 running。
func (a *Agent) markCommandRunning(ctx context.Context, task domain.Task, toolCall *domain.ToolCall) error {
	toolCall.Status = domain.ToolRunning
	return a.record(ctx, task, domain.Transition{ToolCall: toolCall})
}

// logPath 是 <output_dir>/<session>/<task>/<tool_call>.log。工具不知道会话，
// 由 agent 在 Execute 之前填进 Prepared。
func (a *Agent) logPath(task domain.Task, id domain.ToolCallID) string {
	if a.opts.OutputDir == "" {
		return ""
	}
	return filepath.Join(a.opts.OutputDir, string(task.SessionID), string(task.ID), logFileName(id))
}

// logFileName 把 tool_call_id 变成安全的日志文件名。
//
// tool_call_id 来自供应商响应，模型可以构造任意字符串；直接拼进路径会让
// "../../x" 逃出输出目录——capture 会按该路径 MkdirAll 后 O_TRUNC 打开。
// 合法 id 保持原样便于排查；其余用完整 sha256，既不含路径分隔符也不会碰撞。
func logFileName(id domain.ToolCallID) string {
	raw := string(id)
	if safeToolCallID(raw) {
		return raw + ".log"
	}
	sum := sha256.Sum256([]byte(raw))
	return "call_" + hex.EncodeToString(sum[:]) + ".log"
}

// safeToolCallID 只放行日志文件名需要的最小字符集：首字符为字母或数字，
// 其余为字母、数字、'.'、'_'、'-'，长度 1-128。引号、空白、路径分隔符
// 一律走哈希，避免不同平台上的文件名语义差异。
func safeToolCallID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// execAudit 把执行事实拼成一条审计记录。超时与「没跑到有退出码」的执行
// 归到 system：它们不是用户的决定，也不是策略放行。
func execAudit(action, actor string, prepared tools.Prepared, result tools.Result, decision perm.Decision) domain.AuditRecord {
	switch {
	case result.TimedOut:
		action, actor = auditExecTimeout, "system"
	case result.IsError && result.ExitCode == nil:
		action, actor = auditExecFailed, "system"
	}
	return commandAudit(action, actor, prepared, result, decision)
}

// commandAudit 不做超时/失败重分类：拒绝命令时结果里没有退出码，但它不是
// 执行失败，审计里必须写成用户拒绝。
func commandAudit(action, actor string, prepared tools.Prepared, result tools.Result, decision perm.Decision) domain.AuditRecord {
	detail := execAuditDetail{
		BackendVersion: result.BackendVersion,
		PolicyVersion:  decision.PolicyVersion,
		RuleID:         decision.RuleID,
		ExitCode:       result.ExitCode,
		OutputPath:     result.OutputPath,
		LogError:       result.LogError,
		DurationMS:     result.Elapsed.Milliseconds(),
	}
	if !result.Limits.IsZero() {
		limits := result.Limits
		detail.Limits = &limits
	}
	detail.LimitsDegraded = result.LimitsDegraded
	if cmd := prepared.Command; cmd != nil {
		detail.Argv = cmd.Argv
		detail.Cwd = cmd.Cwd
		detail.Backend = cmd.Backend
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = nil
	}
	return domain.AuditRecord{
		Actor:      actor,
		Action:     action,
		ToolCallID: domain.ToolCallID(prepared.Call.ID),
		Detail:     raw,
	}
}

// proposeCommand 走完一次命令审批：请求落库 → 等裁决 → 拒绝/执行。
//
// 与写入审批共用三段流，但不产生 proposal：命令没有「待落盘的内容」，
// 它的可审对象是 argv、cwd、后端与超时。
func (a *Agent) proposeCommand(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	prepared tools.Prepared, decision perm.Decision) (tools.Result, []domain.AuditRecord, error) {

	cmd := prepared.Command
	if cmd == nil {
		return tools.Result{}, nil, fmt.Errorf("agent: 命令审批缺少命令")
	}
	approvalID, err := domain.NewID("apr")
	if err != nil {
		return tools.Result{}, nil, err
	}
	approval := &domain.Approval{
		ID:          domain.ApprovalID(approvalID),
		TaskID:      task.ID,
		ToolCallID:  toolCall.ID,
		Kind:        domain.ApprovalCommand,
		Risk:        domain.RiskHigh,
		RequestedAt: domain.Now(),
	}
	toolCall.ApprovalID = approval.ID
	toolCall.Status = domain.ToolWaitingApprove

	requested, err := domain.NewEvent(domain.EventApprovalRequested, task.SessionID, task.ID,
		domain.ApprovalRequestedPayload{
			ApprovalID: approval.ID,
			Kind:       domain.ApprovalCommand,
			Risk:       domain.RiskHigh,
			Command:    cmd.Argv,
			Cwd:        cmd.Cwd,
			Backend:    cmd.Backend,
			TimeoutMS:  cmd.Timeout.Milliseconds(),
			Reason:     decision.Reason,
		})
	if err != nil {
		return tools.Result{}, nil, err
	}
	// 命令审批期间任务必须显式投影成 waiting_approval：回放与界面都靠这条
	// 状态区分「在等你批准」和「正在执行」。与审批行、事件同事务落库。
	waiting, err := waitingApprovalEvent(task)
	if err != nil {
		return tools.Result{}, nil, err
	}
	if err := a.record(ctx, task, domain.Transition{
		To:       domain.TaskWaitingApproval,
		ToolCall: toolCall,
		Approval: approval,
		Events:   []domain.Event{requested, waiting},
	}); err != nil {
		return tools.Result{}, nil, err
	}

	result, err := a.opts.Recorder.AwaitApproval(ctx, ApprovalRequest{
		SessionID:  task.SessionID,
		ApprovalID: approval.ID,
		Tool:       toolCall.Name,
		Args:       toolCall.Args,
	})
	if err != nil {
		// 任务被取消：审批不能留成永远等不到裁决的 pending。
		_ = a.expireCommandApproval(task, toolCall, approval)
		return tools.Result{}, nil, err
	}

	approval.Decision = result.Decision
	approval.Source = result.Source
	approval.ResolvedAt = domain.Now()
	approval.Note = result.Note

	switch result.Decision {
	case domain.DecisionAllowOnce, domain.DecisionAllowSession:
		resolved, err := domain.NewEvent(domain.EventApprovalResolved, task.SessionID, task.ID,
			domain.ApprovalResolvedPayload{
				ApprovalID: approval.ID,
				Decision:   approval.Decision,
				Source:     approval.Source,
				Note:       approval.Note,
			})
		if err != nil {
			return tools.Result{}, nil, err
		}
		// 裁决是既成事实：即使任务同时被取消，approval.resolved 也要落库，
		// 不能留下一条永远 pending 的审批。同时任务回到 running——审批结束了。
		resumed, rerr := resumeRunningEvent(task)
		if rerr != nil {
			return tools.Result{}, nil, rerr
		}
		if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
			To:       domain.TaskRunning,
			ToolCall: toolCall,
			Approval: approval,
			Events:   []domain.Event{resolved, resumed},
		}); err != nil {
			return tools.Result{}, nil, err
		}
		// 但批准不等于必须执行：任务已取消时进程一个字节都不启动，
		// 由 abortTool 把调用收尾成 canceled。
		if cerr := ctx.Err(); cerr != nil {
			return tools.Result{}, nil, cerr
		}

		prepared.LogPath = a.logPath(task, toolCall.ID)
		// 决定已落定、进程即将启动：投影成 running。执行可能持续几分钟，
		// 让它一直显示「等你批准」会误导界面与事后排查（崩溃时也分不清
		// 是没批准还是跑到一半）。
		if err := a.markCommandRunning(ctx, task, toolCall); err != nil {
			return tools.Result{}, nil, err
		}
		execResult, err := a.opts.Tools.Execute(ctx, prepared)
		if err != nil {
			return tools.Result{}, nil, err
		}
		// 谁做的决定就记谁：allow_session 命中会话白名单时是策略放行，
		// 不能再记成一次人工批准。
		return execResult, []domain.AuditRecord{
			execAudit(auditExecApproved, auditActor(approval.Source), prepared, execResult, decision),
		}, nil
	default:
		// 未知裁决按拒绝处理：默认拒绝比默认放行安全。
		return a.rejectCommand(ctx, task, toolCall, approval, result, prepared, decision)
	}
}

// rejectCommand 处理拒绝：进程一个字节都没有启动。
func (a *Agent) rejectCommand(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	approval *domain.Approval, result ApprovalResult, prepared tools.Prepared, decision perm.Decision) (tools.Result, []domain.AuditRecord, error) {

	toolCall.Status = domain.ToolDenied
	note := result.Note
	if note == "" {
		note = "用户拒绝了这次命令"
	}

	resolved, err := domain.NewEvent(domain.EventApprovalResolved, task.SessionID, task.ID,
		domain.ApprovalResolvedPayload{
			ApprovalID: approval.ID,
			Decision:   approval.Decision,
			Source:     approval.Source,
			Note:       approval.Note,
		})
	if err != nil {
		return tools.Result{}, nil, err
	}
	// 拒绝同理：按下「拒绝」和取消同时发生时，拒绝仍然是有效裁决。
	resumed, err := resumeRunningEvent(task)
	if err != nil {
		return tools.Result{}, nil, err
	}
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		To:       domain.TaskRunning,
		ToolCall: toolCall,
		Approval: approval,
		Events:   []domain.Event{resolved, resumed},
	}); err != nil {
		return tools.Result{}, nil, err
	}

	audit := commandAudit(auditExecDenied, auditActor(approval.Source), prepared, tools.Result{}, decision)
	var shown string
	if cmd := prepared.Command; cmd != nil {
		shown = strings.Join(cmd.Argv, " ")
	}
	return tools.Result{
			Content: fmt.Sprintf("%s（未启动任何进程：%s）", note, shown),
			IsError: true,
		},
		[]domain.AuditRecord{audit},
		nil
}

// expireCommandApproval 把等不到裁决的命令审批收尾。调用时 ctx 已经失效，
// 收尾写入必须换一个不随它取消的 context。
func (a *Agent) expireCommandApproval(task domain.Task, toolCall *domain.ToolCall, approval *domain.Approval) error {
	approval.Decision = domain.DecisionExpired
	approval.Source = domain.SourceSystem
	approval.ResolvedAt = domain.Now()
	approval.Note = "任务已取消"

	toolCall.Status = domain.ToolCanceled
	toolCall.EndedAt = domain.Now()
	toolCall.Error = approval.Note

	ev, err := domain.NewEvent(domain.EventApprovalResolved, task.SessionID, task.ID,
		domain.ApprovalResolvedPayload{
			ApprovalID: approval.ID,
			Decision:   domain.DecisionExpired,
			Source:     domain.SourceSystem,
			Note:       approval.Note,
		})
	if err != nil {
		return err
	}
	// 与写入取消路径一致：没有结果的调用也要有 tool.result，否则历史里会
	// 留下一个没有回填的 tool_use。
	result, err := toolResultEvent(task, toolCall, domain.ToolCanceled, approval.Note+"，命令未执行")
	if err != nil {
		return err
	}
	// 审批收尾后任务回到 running，随后由 abortTool 或 finish 写终态。
	resumed, err := resumeRunningEvent(task)
	if err != nil {
		return err
	}
	return a.record(context.WithoutCancel(context.Background()), task, domain.Transition{
		To:       domain.TaskRunning,
		ToolCall: toolCall,
		Approval: approval,
		Events:   []domain.Event{ev, result, resumed},
	})
}
