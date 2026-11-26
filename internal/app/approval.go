package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
)

// DefaultApprovalTTL 是 allow_session 授权未配置时的有效期。
const DefaultApprovalTTL = 12 * time.Hour

// approvalPollInterval 是等待裁决时轮询 approvals 表的间隔。
//
// 裁决写库而不是投递给进程内的等待者：多运行时下，批准可能发生在另一个
// 进程里，执行任务的协程只能靠轮询来发现（ADR-0050）。400ms 是「人按下
// 按钮到界面生效」与「空闲任务每秒两次读」之间的折中。
const approvalPollInterval = 400 * time.Millisecond

// AwaitApproval 实现 agent.Recorder：阻塞到用户裁决。
//
// 命中会话白名单时立即返回，但仍然由 agent 照常记录 approval.requested 与
// approval.resolved（source=policy）——放行也是一次决定，审计里不能缺。
func (a *App) AwaitApproval(ctx context.Context, req agent.ApprovalRequest) (agent.ApprovalResult, error) {
	if !req.ApprovalID.Valid() {
		return agent.ApprovalResult{}, fmt.Errorf("app: 审批 id 为空")
	}
	// 会话白名单在库里：重启后同一会话的同类调用不该再问一遍。
	// 读失败按「没授权」处理——多问一次比误放行安全，真正的写失败会在
	// ResolveApproval 里暴露出来。
	if approved, err := a.store.SessionApproval(ctx, string(req.SessionID), approvalKey(req.Tool, req.Args), domain.Now()); err == nil && approved {
		return agent.ApprovalResult{Decision: domain.DecisionAllowSession, Source: domain.SourcePolicy}, nil
	}
	// 先看一眼：用户可能在协程走到这里之前就已经批了。
	if res, done, err := a.pollApproval(ctx, req.ApprovalID); err != nil {
		return agent.ApprovalResult{}, err
	} else if done {
		return res, nil
	}

	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return agent.ApprovalResult{}, ctx.Err()
		case <-ticker.C:
			res, done, err := a.pollApproval(ctx, req.ApprovalID)
			if err != nil {
				return agent.ApprovalResult{}, err
			}
			if done {
				return res, nil
			}
		}
	}
}

// pollApproval 读一次库里的裁决。done=false 表示还没人裁决。
//
// 过期视为失败而不是「拒绝」：过期只发生在原协程已经不在的清理场景，
// 这时继续走正常的拒绝流程会再写一遍已经写过的结局。
func (a *App) pollApproval(ctx context.Context, id domain.ApprovalID) (agent.ApprovalResult, bool, error) {
	approval, err := a.store.GetApproval(ctx, id)
	if err != nil {
		return agent.ApprovalResult{}, false, err
	}
	switch approval.Decision {
	case "":
		return agent.ApprovalResult{}, false, nil
	case domain.DecisionExpired:
		return agent.ApprovalResult{}, false, fmt.Errorf("app: 审批 %s 已过期", id)
	}
	return agent.ApprovalResult{
		Decision: approval.Decision,
		Source:   approval.Source,
		Note:     approval.Note,
		Hunks:    approval.Hunks,
	}, true, nil
}

// ResolveApproval 把用户的裁决交给正在等待的协程。
//
// 这里不写任务/提案的状态：终态由 agent 在拿到裁决后自己记录，和 CancelTask
// 的分工一致——「谁拥有这次调用，谁写它的结局」。重复裁决是幂等的空操作。
func (a *App) ResolveApproval(ctx context.Context, sessionID domain.SessionID,
	approvalID domain.ApprovalID, decision domain.Decision, note string, hunks ...int) error {

	switch decision {
	case domain.DecisionAllowOnce, domain.DecisionAllowSession, domain.DecisionDeny:
	default:
		return fmt.Errorf("app: 未知的裁决 %q", decision)
	}
	hunks, err := normalizeHunks(hunks)
	if err != nil {
		return err
	}
	// 部分应用是一次性决定：同一份 diff 的其它块下次还得单独看，
	// 所以不写会话白名单，按 allow_once 处理。
	if decision == domain.DecisionAllowSession && len(hunks) > 0 {
		decision = domain.DecisionAllowOnce
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

	// CAS 写库：两个进程同时裁决时只有一个成功，另一个按幂等处理。
	// 返回 true 才轮得到写会话白名单，避免「别人批的」被重复记一次。
	sticks, err := a.store.ResolveApprovalDecision(ctx, approvalID, decision,
		domain.SourceUser, note, domain.Now(), hunks)
	if err != nil {
		return err
	}
	if !sticks {
		return nil
	}

	if decision == domain.DecisionAllowSession && len(hunks) == 0 {
		call, err := a.store.GetToolCall(ctx, approval.ToolCallID)
		if err != nil {
			return err
		}
		now := domain.Now()
		if err := a.store.RememberSessionApproval(ctx, string(task.SessionID),
			approvalKey(call.Name, call.Args), now, now.Add(a.approvalTTL())); err != nil {
			return err
		}
	}
	return nil
}

// approvalTTL 是 allow_session 授权的有效期。未配置或非正数时用默认 12 小时：
// 便利可以有，但不能变成一条永不过期的豁免。
func (a *App) approvalTTL() time.Duration {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if a.sessionApprovalTTL > 0 {
		return a.sessionApprovalTTL
	}
	return DefaultApprovalTTL
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
		// 别的运行时还活着：这条审批有人等，结局由它自己写。多运行时下
		// 启动清理不能把别人正在等的审批标成过期（ADR-0049/0050）。
		if task.Owner != "" && task.Owner != a.owner && a.runtimeAlive(ctx, task.Owner) {
			continue
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
				// 与取消路径一致：每个 tool_use 都要有一条 tool.result。
				// 少了它，重启后回放出的历史带着孤立的工具调用，下一次请求
				// 会被供应商直接拒绝。
				resultEv, rerr := domain.NewEvent(domain.EventToolResult, task.SessionID, task.ID,
					domain.ToolResultPayload{
						Tool:       call.Name,
						ToolCallID: string(call.ID),
						Status:     string(domain.ToolCanceled),
						IsError:    true,
						Output:     approval.Note + "，工具未执行",
					})
				if rerr != nil {
					return rerr
				}
				tr.Events = append(tr.Events, resultEv)
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
	return a.expireProposalOfWithNote(ctx, task, callID, "进程重启，提案已失效")
}

// expireProposalOfWithNote 按指定原因把提案标成 stale。note 会进事件，调用方
// 要写清「为什么这个提案没有落盘」。
func (a *App) expireProposalOfWithNote(ctx context.Context, task domain.Task, callID domain.ToolCallID, note string) error {
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
			Reason:     note,
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

// normalizeHunks 去重并升序；负下标是错误。空输入返回 nil（整份应用）。
func normalizeHunks(in []int) ([]int, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, idx := range in {
		if idx < 0 {
			return nil, fmt.Errorf("app: 块下标不能为负数（%d）", idx)
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, idx)
	}
	sort.Ints(out)
	return out, nil
}
