package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// reapNote 是启动收尾写进工具调用与 tool.result 的原因。
const reapNote = "进程重启，工具调用未完成"

// reapUnknownNote 用于「审批已裁决、但缺 tool.result」的调用。
//
// 这类调用大概率已经开始执行（命令审批在执行前就落定了），结果无从判定。
// 补一条 canceled + 明说「结果未知」，比什么都不写更接近事实：不回填的话
// 回放出的历史带着孤立的 tool_use，下一次请求会被供应商直接拒绝，整个会话
// 就此不可用。
const reapUnknownNote = "执行中进程重启，结果未知"

// ReapUnfinished 收尾上一次进程留下的未完成状态：停在半路、却没有 tool.result
// 的工具调用，以及不再有协程推进的幽灵任务。
//
// ExpirePendingApprovals 只覆盖得到「有 pending 审批」的那批。崩溃时正在跑
// read_file 或命令、或者刚写完 tool.prepared 还没来得及写审批的调用没有审批行
// 可查，会永远停在 prepared/running；回放会把它们还原成一个没有结果的
// tool_use，下一次请求直接失败，所以必须在这里收尾。
//
// 已裁决但没有结果的调用同样要补：它跑没跑无从判断，但「没有结果」这件事
// 本身是确定的，补 canceled 并注明结果未知，既保住历史结构，也不假装知道
// 执行结果。CheckApprovalConsistency 仍留作安全网（例如这里补写失败时）。
func (a *App) ReapUnfinished(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	calls, err := a.store.UnfinishedToolCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		decided, err := a.approvalDecided(ctx, call)
		if err != nil {
			return err
		}
		note := reapNote
		if decided {
			note = reapUnknownNote
		}
		call.Status = domain.ToolCanceled
		call.EndedAt = domain.Now()
		call.Error = note
		ev, err := domain.NewEvent(domain.EventToolResult, call.SessionID, call.TaskID,
			domain.ToolResultPayload{
				Tool:       call.Name,
				ToolCallID: string(call.ID),
				Status:     string(domain.ToolCanceled),
				IsError:    true,
				Output:     note + "，未产生结果",
			})
		if err != nil {
			return err
		}
		if err := a.commit(func() ([]domain.Event, error) {
			return a.store.Apply(ctx, domain.Transition{
				TaskID:   call.TaskID,
				ToolCall: &call,
				Events:   []domain.Event{ev},
			})
		}); err != nil {
			return err
		}
		if decided {
			logger.Warn("启动收尾已裁决但缺结果的工具调用（结果未知）",
				"task", call.TaskID, "tool_call", call.ID, "tool", call.Name)
		} else {
			logger.Warn("启动收尾未完成的工具调用",
				"task", call.TaskID, "tool_call", call.ID, "tool", call.Name)
		}
	}

	tasks, err := a.store.UnfinishedTasks(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.Status.Terminal() {
			continue
		}
		// 没有审批行的幽灵任务（崩溃时在等模型流式输出、或在跑只读工具）
		// 同样要收尾，否则会话里永远挂着一条 running。
		if err := a.cancelGhost(ctx, task, "进程重启，任务已失效"); err != nil {
			return err
		}
		logger.Warn("启动取消遗留任务", "task", task.ID, "status", task.Status)
	}
	return nil
}

// approvalDecided 报告这次调用的审批是否已经有裁决。
//
// 查不到审批行不算裁决：那说明进程在写审批之前就死了，调用确实没有等到
// 任何决定。
func (a *App) approvalDecided(ctx context.Context, call domain.ToolCall) (bool, error) {
	if call.ApprovalID == "" {
		return false, nil
	}
	approval, err := a.store.GetApproval(ctx, call.ApprovalID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return approval.Decision != "", nil
}
