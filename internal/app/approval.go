package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
)

// approvalState 保存审批的等待者与会话级白名单。
//
// 这两样都只在内存里：等待者依附于正在运行的协程，进程一死就没有意义；
// allow_session 是「本次会话内别再问我」的临时便利，落盘会让它变成一条
// 永久豁免，而 AGENTS 的核心主张恰恰是「每次写入都要有人负责」。
type approvalState struct {
	mu      sync.Mutex
	waiters map[domain.ApprovalID]*approvalWait
	// resolved 暂存「已经裁决、但等待的协程还没来取」的结果。
	// 没有它就有丢裁决的窗口：事件先广播，用户可能比协程先看到并按下 y。
	resolved map[domain.ApprovalID]agent.ApprovalResult
	// sessions[session][key] 是已放行的 tool+args 摘要。
	sessions map[domain.SessionID]map[string]bool
}

type approvalWait struct {
	sessionID domain.SessionID
	ch        chan agent.ApprovalResult
}

// AwaitApproval 实现 agent.Recorder：阻塞到用户裁决。
//
// 命中会话白名单时立即返回，但仍然由 agent 照常记录 approval.requested 与
// approval.resolved（source=policy）——放行也是一次决定，审计里不能缺。
func (a *App) AwaitApproval(ctx context.Context, req agent.ApprovalRequest) (agent.ApprovalResult, error) {
	if !req.ApprovalID.Valid() {
		return agent.ApprovalResult{}, fmt.Errorf("app: 审批 id 为空")
	}
	key := approvalKey(req.Tool, req.Args)

	a.approvals.mu.Lock()
	if a.approvals.sessions[req.SessionID][key] {
		a.approvals.mu.Unlock()
		return agent.ApprovalResult{Decision: domain.DecisionAllowSession, Source: domain.SourcePolicy}, nil
	}
	if done, ok := a.approvals.resolved[req.ApprovalID]; ok {
		delete(a.approvals.resolved, req.ApprovalID)
		a.approvals.mu.Unlock()
		return done, nil
	}
	wait := &approvalWait{sessionID: req.SessionID, ch: make(chan agent.ApprovalResult, 1)}
	if a.approvals.waiters == nil {
		a.approvals.waiters = make(map[domain.ApprovalID]*approvalWait)
	}
	a.approvals.waiters[req.ApprovalID] = wait
	a.approvals.mu.Unlock()

	defer func() {
		a.approvals.mu.Lock()
		if a.approvals.waiters[req.ApprovalID] == wait {
			delete(a.approvals.waiters, req.ApprovalID)
		}
		a.approvals.mu.Unlock()
	}()

	select {
	case res := <-wait.ch:
		return res, nil
	case <-ctx.Done():
		return agent.ApprovalResult{}, ctx.Err()
	}
}

// ResolveApproval 把用户的裁决交给正在等待的协程。
//
// 这里不写任务/提案的状态：终态由 agent 在拿到裁决后自己记录，和 CancelTask
// 的分工一致——「谁拥有这次调用，谁写它的结局」。重复裁决是幂等的空操作。
func (a *App) ResolveApproval(ctx context.Context, sessionID domain.SessionID,
	approvalID domain.ApprovalID, decision domain.Decision, note string) error {

	switch decision {
	case domain.DecisionAllowOnce, domain.DecisionAllowSession, domain.DecisionDeny:
	default:
		return fmt.Errorf("app: 未知的裁决 %q", decision)
	}

	approval, err := a.store.GetApproval(ctx, approvalID)
	if err != nil {
		return err
	}
	if approval.Decision != "" {
		return nil
	}
	task, err := a.store.GetTask(ctx, approval.TaskID)
	if err != nil {
		return err
	}
	// approvals 表没有 session 列：用所属任务反查，顺手校验客户端没串会话。
	if sessionID.Valid() && task.SessionID != sessionID {
		return fmt.Errorf("app: 审批 %s 不属于会话 %s: %w", approvalID, sessionID, domain.ErrNotFound)
	}

	result := agent.ApprovalResult{Decision: decision, Source: domain.SourceUser, Note: note}
	a.approvals.mu.Lock()
	if wait := a.approvals.waiters[approvalID]; wait != nil {
		select {
		case wait.ch <- result:
		default:
			// 已经有人投递过：幂等，忽略。
		}
	} else {
		if a.approvals.resolved == nil {
			a.approvals.resolved = make(map[domain.ApprovalID]agent.ApprovalResult)
		}
		a.approvals.resolved[approvalID] = result
	}
	a.approvals.mu.Unlock()

	if decision == domain.DecisionAllowSession {
		call, err := a.store.GetToolCall(ctx, approval.ToolCallID)
		if err != nil {
			return err
		}
		a.rememberApproval(task.SessionID, approvalKey(call.Name, call.Args))
	}
	return nil
}

func (a *App) rememberApproval(sessionID domain.SessionID, key string) {
	a.approvals.mu.Lock()
	defer a.approvals.mu.Unlock()
	if a.approvals.sessions == nil {
		a.approvals.sessions = make(map[domain.SessionID]map[string]bool)
	}
	set := a.approvals.sessions[sessionID]
	if set == nil {
		set = make(map[string]bool)
		a.approvals.sessions[sessionID] = set
	}
	set[key] = true
}

// approvalKey 是 allow_session 的键：工具名 + 参数原文的摘要。
//
// 用原文而不是解析后的结构：参数顺序、空白都可能不同，但「同一段参数原文」
// 一定是同一次请求，宁可多问一次也不要误放行。
func approvalKey(tool string, args []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), args...))
	return hex.EncodeToString(sum[:])
}

// ExpirePendingApprovals 清理上一次进程留下的待审批：标成 expired、提案标
// stale、幽灵任务标 canceled。
//
// v1 不自动续跑（ADR-0007）：这些审批对应的协程已经不在了，留着它们只会让
// 会话看起来永远卡在「等你批准」。
func (a *App) ExpirePendingApprovals(ctx context.Context) error {
	pending, err := a.store.PendingApprovals(ctx)
	if err != nil {
		return err
	}
	for _, approval := range pending {
		task, err := a.store.GetTask(ctx, approval.TaskID)
		if err != nil {
			return err
		}
		approval.Decision = domain.DecisionExpired
		approval.Source = domain.SourceSystem
		approval.ResolvedAt = domain.Now()
		if approval.Note == "" {
			approval.Note = "进程重启，审批已失效"
		}
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
		tr := domain.Transition{TaskID: approval.TaskID, Approval: &approval, Events: []domain.Event{ev}}
		// 工具调用也要收尾，否则 tool_calls 永远停在 waiting_approval。
		// 历史库里可能查不到引用的 tool_call_id，那种情况按「没有就跳过」处理：
		// 一条脏引用不该挡住整个启动清理。
		call, err := a.store.GetToolCall(ctx, approval.ToolCallID)
		switch {
		case err == nil:
			if call.Status == domain.ToolWaitingApprove {
				call.Status = domain.ToolCanceled
				call.EndedAt = domain.Now()
				call.Error = approval.Note
				tr.ToolCall = &call
			}
		case errors.Is(err, domain.ErrNotFound):
		default:
			return err
		}
		if err := a.commit(func() ([]domain.Event, error) { return a.store.Apply(ctx, tr) }); err != nil {
			return err
		}
		if err := a.expireProposalOf(ctx, task, approval.ToolCallID); err != nil {
			return err
		}
		if !task.Status.Terminal() {
			if err := a.cancelGhost(ctx, task, "进程重启，任务已失效"); err != nil {
				return err
			}
		}
	}
	return nil
}

// expireProposalOf 把该审批对应的提案标成 stale。
func (a *App) expireProposalOf(ctx context.Context, task domain.Task, callID domain.ToolCallID) error {
	proposal, err := a.store.ProposalByToolCall(ctx, callID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	if proposal.Status != domain.ProposalPending {
		return nil
	}
	proposal.Status = domain.ProposalStale
	ev, err := domain.NewEvent(domain.EventProposalRejected, task.SessionID, task.ID,
		domain.ProposalRejectedPayload{
			ProposalID: proposal.ID,
			Path:       proposal.Path,
			Status:     proposal.Status,
			Reason:     "进程重启，提案已失效",
		})
	if err != nil {
		return err
	}
	return a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, domain.Transition{
			TaskID:   task.ID,
			Proposal: &proposal,
			Events:   []domain.Event{ev},
		})
	})
}

// cancelGhost 给没有协程在跑的任务补一条 canceled 终态。
func (a *App) cancelGhost(ctx context.Context, task domain.Task, reason string) error {
	ev, err := domain.NewEvent(domain.EventTaskStatus, task.SessionID, task.ID, domain.TaskStatusPayload{
		From:   task.Status,
		To:     domain.TaskCanceled,
		Reason: "canceled",
		Error:  reason,
	})
	if err != nil {
		return err
	}
	return a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, domain.Transition{
			TaskID: task.ID,
			To:     domain.TaskCanceled,
			Events: []domain.Event{ev},
		})
	})
}
