package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// proposeChange 走完一次写入审批：提案落库 → 等裁决 → 拒绝/过期/落盘。
//
// 每个分支只写一次原子迁移（投影 + 事件同事务）。落盘本身不产生事件，
// 真正的写入结果是随后的 proposal.applied 或 proposal.stale。
func (a *Agent) proposeChange(ctx context.Context, task domain.Task, toolCall *domain.ToolCall, prepared tools.Prepared) (tools.Result, error) {
	change := prepared.Change
	approvalID, err := domain.NewID("apr")
	if err != nil {
		return tools.Result{}, err
	}
	proposalID, err := domain.NewID("prp")
	if err != nil {
		return tools.Result{}, err
	}

	approval := &domain.Approval{
		ID:          domain.ApprovalID(approvalID),
		TaskID:      task.ID,
		ToolCallID:  toolCall.ID,
		Kind:        domain.ApprovalDiff,
		Risk:        domain.RiskMedium,
		RequestedAt: domain.Now(),
	}
	proposal := &domain.Proposal{
		ID:         domain.ProposalID(proposalID),
		TaskID:     task.ID,
		ToolCallID: toolCall.ID,
		Path:       change.Path,
		Diff:       change.Diff,
		BaseHash:   change.BaseHash,
		NewHash:    change.NewHash,
		Status:     domain.ProposalPending,
	}
	toolCall.ApprovalID = approval.ID

	created, err := domain.NewEvent(domain.EventProposalCreated, task.SessionID, task.ID,
		domain.ProposalCreatedPayload{
			ProposalID: proposal.ID,
			Path:       change.Path,
			BaseHash:   change.BaseHash,
		})
	if err != nil {
		return tools.Result{}, err
	}
	// diff 只挂在 approval.requested 上：两条事件都带一份完整 diff 会让
	// 事件日志凭空翻倍。
	requested, err := domain.NewEvent(domain.EventApprovalRequested, task.SessionID, task.ID,
		domain.ApprovalRequestedPayload{
			ApprovalID: approval.ID,
			Kind:       domain.ApprovalDiff,
			Risk:       domain.RiskMedium,
			Path:       change.Path,
			Diff:       change.Diff,
			Truncated:  change.Truncated,
		})
	if err != nil {
		return tools.Result{}, err
	}
	if err := a.record(ctx, task, domain.Transition{
		ToolCall: toolCall,
		Approval: approval,
		Proposal: proposal,
		Events:   []domain.Event{created, requested},
	}); err != nil {
		return tools.Result{}, err
	}

	decision, err := a.opts.Recorder.AwaitApproval(ctx, ApprovalRequest{
		SessionID:  task.SessionID,
		ApprovalID: approval.ID,
		Tool:       toolCall.Name,
		Args:       toolCall.Args,
	})
	if err != nil {
		// 任务被取消：审批不能留成「永远等不到裁决」的 pending。
		// ctx 已经废了，收尾写入必须换一个不随它取消的 context。
		_ = a.expireApproval(task, toolCall, approval, proposal)
		return tools.Result{}, err
	}

	approval.Decision = decision.Decision
	approval.Source = decision.Source
	approval.ResolvedAt = domain.Now()
	approval.Note = decision.Note

	switch decision.Decision {
	case domain.DecisionAllowOnce, domain.DecisionAllowSession:
		return a.applyChange(ctx, task, toolCall, approval, proposal, change)
	default:
		// 未知裁决按拒绝处理：默认拒绝比默认放行安全。
		return a.rejectChange(ctx, task, toolCall, approval, proposal, decision, change)
	}
}

// rejectChange 处理拒绝：磁盘一个字节都不动。
func (a *Agent) rejectChange(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	approval *domain.Approval, proposal *domain.Proposal, decision ApprovalResult, change *tools.Change) (tools.Result, error) {

	proposal.Status = domain.ProposalRejected
	toolCall.Status = domain.ToolDenied

	note := decision.Note
	if note == "" {
		note = "用户拒绝了这次写入"
	}
	resolved, err := domain.NewEvent(domain.EventApprovalResolved, task.SessionID, task.ID,
		domain.ApprovalResolvedPayload{
			ApprovalID: approval.ID,
			Decision:   approval.Decision,
			Source:     approval.Source,
			Note:       approval.Note,
		})
	if err != nil {
		return tools.Result{}, err
	}
	// 用户写的理由要原样进事件与审计：这两处是事后还原「为什么没写」的唯一
	// 依据，不能用一句固定的「用户拒绝」把它盖掉。
	rejected, err := rejectedEvent(task, proposal, note)
	if err != nil {
		return tools.Result{}, err
	}
	// 用户可能按下回车的同时取消任务：拒绝是既成事实，不能因为 ctx 一起
	// 失效就退回 pending。
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		ToolCall: toolCall,
		Approval: approval,
		Proposal: proposal,
		Audit: []domain.AuditRecord{{
			Actor:      auditActor(approval.Source),
			Action:     auditToolDenied,
			ToolCallID: toolCall.ID,
			Detail:     rejectedDetail(change.Path, note),
		}},
		Events: []domain.Event{resolved, rejected},
	}); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{
		Content: fmt.Sprintf("%s（%s 未被修改）", note, change.Path),
		IsError: true,
	}, nil
}

// applyChange 处理批准：先做 CAS 检查，再原子落盘。
func (a *Agent) applyChange(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	approval *domain.Approval, proposal *domain.Proposal, change *tools.Change) (tools.Result, error) {

	resolved, err := domain.NewEvent(domain.EventApprovalResolved, task.SessionID, task.ID,
		domain.ApprovalResolvedPayload{
			ApprovalID: approval.ID,
			Decision:   approval.Decision,
			Source:     approval.Source,
			Note:       approval.Note,
		})
	if err != nil {
		return tools.Result{}, err
	}

	// 裁决落定与取消撞在一起：批准是既成事实，必须落成 allow 而不是 expired；
	// 但任务已经取消，不能再动磁盘。此时把提案标 stale、调用标 canceled，
	// 并补一条 tool.result——否则审批永远 pending，历史里也少一条 tool_result。
	if cerr := ctx.Err(); cerr != nil {
		note := "任务已取消，写入未发生"
		proposal.Status = domain.ProposalStale
		toolCall.Status = domain.ToolCanceled
		toolCall.EndedAt = domain.Now()
		toolCall.Error = note
		rejected, rerr := rejectedEvent(task, proposal, note)
		if rerr != nil {
			return tools.Result{}, rerr
		}
		result, rerr := toolResultEvent(task, toolCall, domain.ToolCanceled, note)
		if rerr != nil {
			return tools.Result{}, rerr
		}
		if rerr := a.record(context.WithoutCancel(ctx), task, domain.Transition{
			ToolCall: toolCall,
			Approval: approval,
			Proposal: proposal,
			Events:   []domain.Event{resolved, rejected, result},
		}); rerr != nil {
			return tools.Result{}, rerr
		}
		return tools.Result{}, cerr
	}

	// CAS：用户审阅期间文件可能被别的进程改过。此时覆盖会静默丢掉别人的
	// 改动，所以拒绝并要求模型重新读、重新出 diff。
	hash, exists, err := a.opts.Workspace.Hash(change.Path)
	if err != nil {
		return a.failChange(ctx, task, toolCall, approval, proposal, resolved, change,
			fmt.Sprintf("检查 %s 的当前内容失败：%v", change.Path, err))
	}
	if exists != change.Exists || hash != change.BaseHash {
		proposal.Status = domain.ProposalStale
		toolCall.Status = domain.ToolFailed
		rejected, err := rejectedEvent(task, proposal, "base_hash 不匹配")
		if err != nil {
			return tools.Result{}, err
		}
		if rerr := a.record(context.WithoutCancel(ctx), task, domain.Transition{
			ToolCall: toolCall,
			Approval: approval,
			Proposal: proposal,
			Events:   []domain.Event{resolved, rejected},
		}); rerr != nil {
			return tools.Result{}, rerr
		}
		return tools.Result{
			Content: fmt.Sprintf("stale_proposal：%s 在审批期间被修改过（期望 %s，实际 %s）。"+
				"请重新 read_file 后再提出改动", change.Path, patch.ShortHash(change.BaseHash),
				patch.ShortHash(hash)),
			IsError: true,
		}, nil
	}

	if err := a.opts.Workspace.Write(change.Path, change.NewContent); err != nil {
		return a.failChange(ctx, task, toolCall, approval, proposal, resolved, change,
			fmt.Sprintf("写入 %s 失败：%v", change.Path, err))
	}

	proposal.Status = domain.ProposalApplied
	proposal.AppliedAt = domain.Now()
	toolCall.Status = domain.ToolSucceeded

	applied, err := domain.NewEvent(domain.EventProposalApplied, task.SessionID, task.ID,
		domain.ProposalAppliedPayload{
			ProposalID: proposal.ID,
			Path:       change.Path,
			NewHash:    change.NewHash,
		})
	if err != nil {
		return tools.Result{}, err
	}
	// 磁盘已经变了：这条结局必须落库，哪怕任务在同一瞬间被取消。
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		ToolCall: toolCall,
		Approval: approval,
		Proposal: proposal,
		Events:   []domain.Event{resolved, applied},
	}); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: describeApproved(change)}, nil
}

// failChange 处理「批准了但写不进去」：审批仍然算已裁决，提案标 failed。
func (a *Agent) failChange(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	approval *domain.Approval, proposal *domain.Proposal, resolved domain.Event,
	change *tools.Change, message string) (tools.Result, error) {

	proposal.Status = domain.ProposalFailed
	toolCall.Status = domain.ToolFailed
	rejected, err := rejectedEvent(task, proposal, message)
	if err != nil {
		return tools.Result{}, err
	}
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		ToolCall: toolCall,
		Approval: approval,
		Proposal: proposal,
		Events:   []domain.Event{resolved, rejected},
	}); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: message + "；文件未被修改", IsError: true}, nil
}

// expireApproval 把等不到裁决的审批收尾。调用时 ctx 已经失效，只能另起一个。
//
// 三条投影必须一起走：审批 expired、提案 stale、工具调用 canceled。
// 只写审批会让 tool_calls 永远停在 waiting_approval——回放和审计里会看到一次
// 「已准备、然后消失」的调用。
func (a *Agent) expireApproval(task domain.Task, toolCall *domain.ToolCall,
	approval *domain.Approval, proposal *domain.Proposal) error {

	approval.Decision = domain.DecisionExpired
	approval.Source = domain.SourceSystem
	approval.ResolvedAt = domain.Now()
	approval.Note = "任务已取消"

	proposal.Status = domain.ProposalStale
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
	// 提案的终态必须进事件日志：只改投影会让回放看到一条永远 pending 的提案。
	rejected, err := rejectedEvent(task, proposal, approval.Note)
	if err != nil {
		return err
	}
	// 这次调用没有结果就结束了：必须补一条 tool.result，否则历史里会留下
	// 一个没有回填的 tool_use，下一次请求直接失败。
	result, err := toolResultEvent(task, toolCall, domain.ToolCanceled, approval.Note+"，写入未发生")
	if err != nil {
		return err
	}
	// 不跟随已取消的 ctx：否则这条收尾写入自己也写不进去。
	return a.record(context.WithoutCancel(context.Background()), task, domain.Transition{
		ToolCall: toolCall,
		Approval: approval,
		Proposal: proposal,
		Events:   []domain.Event{ev, rejected, result},
	})
}

// rejectedDetail 是「这次调用没有发生」的审计事实：路径与原因。
// 没有路径的拒绝（例如策略拒绝执行没有可审内容的工具）不写空字段。
func rejectedDetail(path, reason string) json.RawMessage {
	detail := map[string]string{"reason": reason}
	if path != "" {
		detail["path"] = path
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil
	}
	return raw
}

// auditActor 把审批来源映射成审计的 actor：谁做的决定就记谁，
// 不能一律写成 user——策略拒绝与系统收尾都不是「用户拒绝了」。
func auditActor(source domain.ApprovalSource) string {
	if source == "" {
		return string(domain.SourceUser)
	}
	return string(source)
}

// rejectedEvent 记录「这次写入没有发生」以及落在哪种结局上。
func rejectedEvent(task domain.Task, p *domain.Proposal, reason string) (domain.Event, error) {
	return domain.NewEvent(domain.EventProposalRejected, task.SessionID, task.ID,
		domain.ProposalRejectedPayload{
			ProposalID: p.ID,
			Path:       p.Path,
			Status:     p.Status,
			Reason:     reason,
		})
}
