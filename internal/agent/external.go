package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// 外部工具（MCP / 插件）的审计动作。与命令审计分开：事后要能一眼看出
// 「这是宿主自己跑的，还是交给了外面」。
const (
	auditExternalAllow    = "external.allow"
	auditExternalApproved = "external.approved"
	auditExternalDenied   = "external.denied"
	auditExternalFailed   = "external.failed"
)

// externalAuditDetail 记录一次外部调用的事实。宿主拿不到退出码，能承诺的
// 只有「调了谁、带什么参数、依据哪套规则、跑了多久」。
type externalAuditDetail struct {
	Tool          string          `json:"tool"`
	Source        string          `json:"source"`
	Args          json.RawMessage `json:"args,omitempty"`
	PolicyVersion string          `json:"policy_version,omitempty"`
	RuleID        string          `json:"rule_id,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	DurationMS    int64           `json:"duration_ms,omitempty"`
}

func externalAudit(action, actor string, prepared tools.Prepared, decision perm.Decision, result tools.Result) domain.AuditRecord {
	detail := externalAuditDetail{
		Tool:          prepared.Call.Name,
		PolicyVersion: decision.PolicyVersion,
		RuleID:        decision.RuleID,
		Reason:        decision.Reason,
		DurationMS:    result.Elapsed.Milliseconds(),
	}
	if prepared.External != nil {
		detail.Source = prepared.External.Source
	}
	detail.Args = clipArgs(prepared.Call.Args)
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = json.RawMessage(`{}`)
	}
	return domain.AuditRecord{
		Actor:      actor,
		Action:     action,
		ToolCallID: domain.ToolCallID(prepared.Call.ID),
		Detail:     raw,
	}
}

// clipArgs 给审批事件与审计留一份参数快照，但不允许它无界增长。
const maxExternalArgsBytes = 8 << 10

func clipArgs(args json.RawMessage) json.RawMessage {
	if len(args) <= maxExternalArgsBytes {
		return args
	}
	out := make([]byte, 0, maxExternalArgsBytes+32)
	out = append(out, args[:maxExternalArgsBytes]...)
	out = append(out, `…（参数已截断）`...)
	return out
}

// proposeExternal 发起一次外部工具审批。
//
// 与 proposeCommand 同构：先落审批行与 waiting_approval，再阻塞等裁决；
// 批准后才调用 Execute（外部进程此刻才收到 tools/call），拒绝则一个字节
// 都不会发给外部工具。
func (a *Agent) proposeExternal(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	prepared tools.Prepared, decision perm.Decision) (tools.Result, []domain.AuditRecord, error) {

	ext := prepared.External
	if ext == nil {
		return tools.Result{}, nil, fmt.Errorf("agent: 外部工具审批缺少来源")
	}
	approvalID, err := domain.NewID("apr")
	if err != nil {
		return tools.Result{}, nil, err
	}
	approval := &domain.Approval{
		ID:          domain.ApprovalID(approvalID),
		TaskID:      task.ID,
		ToolCallID:  toolCall.ID,
		Kind:        domain.ApprovalExternal,
		Risk:        domain.RiskHigh,
		RequestedAt: domain.Now(),
	}
	toolCall.ApprovalID = approval.ID
	toolCall.Status = domain.ToolWaitingApprove

	requested, err := domain.NewEvent(domain.EventApprovalRequested, task.SessionID, task.ID,
		domain.ApprovalRequestedPayload{
			ApprovalID: approval.ID,
			Kind:       domain.ApprovalExternal,
			Risk:       domain.RiskHigh,
			Tool:       prepared.Call.Name,
			Source:     ext.Source,
			Args:       string(clipArgs(prepared.Call.Args)),
			Reason:     decision.Reason,
		})
	if err != nil {
		return tools.Result{}, nil, err
	}
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
		_ = a.expireExternalApproval(task, toolCall, approval)
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
		// 批准不等于必须执行：任务已取消时不再给外部进程发请求。
		if cerr := ctx.Err(); cerr != nil {
			return tools.Result{}, nil, cerr
		}

		if err := a.markCommandRunning(ctx, task, toolCall); err != nil {
			return tools.Result{}, nil, err
		}
		execResult, err := a.opts.Tools.Execute(ctx, prepared)
		if err != nil {
			return tools.Result{}, nil, err
		}
		return execResult, []domain.AuditRecord{
			externalAudit(auditExternalApproved, auditActor(approval.Source), prepared, decision, execResult),
		}, nil
	default:
		return a.rejectExternal(ctx, task, toolCall, approval, result, prepared, decision)
	}
}

// rejectExternal 处理拒绝：外部工具一个请求都没收到。
func (a *Agent) rejectExternal(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	approval *domain.Approval, result ApprovalResult, prepared tools.Prepared, decision perm.Decision) (tools.Result, []domain.AuditRecord, error) {

	toolCall.Status = domain.ToolDenied
	note := result.Note
	if note == "" {
		note = "用户拒绝了这次外部工具调用"
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

	return tools.Result{
			Content: fmt.Sprintf("%s（未调用外部工具：%s）", note, prepared.Call.Name),
			IsError: true,
		},
		[]domain.AuditRecord{externalAudit(auditExternalDenied, auditActor(approval.Source), prepared, decision, tools.Result{})},
		nil
}

// expireExternalApproval 把等不到裁决的外部工具审批收尾。
func (a *Agent) expireExternalApproval(task domain.Task, toolCall *domain.ToolCall, approval *domain.Approval) error {
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
	result, err := toolResultEvent(task, toolCall, domain.ToolCanceled, approval.Note+"，外部工具未调用")
	if err != nil {
		return err
	}
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
