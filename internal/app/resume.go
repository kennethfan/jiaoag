package app

import (
	"context"
	"log/slog"
	"sort"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// resumeFailedReason 是重启恢复失败时写进 task.status 的 reason。
const resumeFailedReason = "resume_failed"

// resumeQueuedTasks 把上次进程留下的 queued 任务重新装进内存队列并拉起。
//
// 只恢复 queued：running / waiting_approval 的任务可能已经产生副作用，重跑
// 会让工具执行两次；它们在 ReapUnfinished 里按 canceled 收尾。queued 的任务
// 从未启动，重拉是安全的。
func (a *App) resumeQueuedTasks(ctx context.Context, deps *RuntimeDeps) error {
	logger := deps.Logger
	tasks, err := a.store.UnfinishedTasks(ctx)
	if err != nil {
		return err
	}
	var queued []domain.Task
	for _, task := range tasks {
		if task.Status == domain.TaskQueued {
			queued = append(queued, task)
		}
	}
	if len(queued) == 0 {
		return nil
	}
	// 按提交顺序恢复：同一会话内的先后关系就是用户看到的排队顺序。
	// CreatedAt 相同（同一毫秒提交）时用 ID 兜底，保证顺序稳定。
	sort.SliceStable(queued, func(i, j int) bool {
		if !queued[i].CreatedAt.Equal(queued[j].CreatedAt) {
			return queued[i].CreatedAt.Before(queued[j].CreatedAt)
		}
		return queued[i].ID < queued[j].ID
	})

	var sessions []domain.SessionID
	seen := make(map[domain.SessionID]bool)
	for _, task := range queued {
		sess, err := a.store.GetSession(ctx, task.SessionID)
		if err != nil {
			logger.Error("恢复排队任务：读取会话失败",
				"task", task.ID, "session", task.SessionID, "err", err)
			a.failResumedTask(ctx, logger, task, "读取会话失败: "+err.Error())
			continue
		}
		ag, _, digest, err := a.newAgent(sess, deps, task.Skills)
		if err != nil {
			logger.Error("恢复排队任务：重建执行环境失败", "task", task.ID, "err", err)
			a.failResumedTask(ctx, logger, task, "重建执行环境失败: "+err.Error())
			continue
		}
		// 配置在两次启动之间可能变了（工具集、规则集、后端、系统提示）。
		// 任务里冻结的指纹只用于事后解释「当时是哪套组合」，这里不阻断恢复，
		// 但必须留下痕迹，否则审计里两次启动的差异无从解释。
		if task.ExtensionsDigest != "" && digest != task.ExtensionsDigest {
			logger.Warn("重启恢复时扩展指纹已变化",
				"task", task.ID, "recorded", task.ExtensionsDigest, "current", digest)
		}
		a.runtimeMu.Lock()
		if a.closed {
			a.runtimeMu.Unlock()
			return nil
		}
		a.enqueueLocked(pendingTask{sess: sess, task: task, ag: ag, deps: deps})
		a.runtimeMu.Unlock()
		if !seen[sess.ID] {
			seen[sess.ID] = true
			sessions = append(sessions, sess.ID)
		}
	}
	// 每个会话拉起队首；startLocked 自己会抢全局并发槽，抢不到就把任务
	// 留在队首，等其它任务让出槽后由 pump 补位。
	for _, sessionID := range sessions {
		a.runtimeMu.Lock()
		a.startNextLocked(sessionID)
		a.runtimeMu.Unlock()
	}
	logger.Info("已恢复排队任务", "tasks", len(queued), "sessions", len(sessions))
	return nil
}

// failResumedTask 把无法恢复的排队任务标成 failed，避免它永远停在 queued。
func (a *App) failResumedTask(ctx context.Context, logger *slog.Logger, task domain.Task, note string) {
	ev, err := domain.NewEvent(domain.EventTaskStatus, task.SessionID, task.ID, domain.TaskStatusPayload{
		From:   task.Status,
		To:     domain.TaskFailed,
		Reason: resumeFailedReason,
		Error:  note,
	})
	if err != nil {
		logger.Error("构造恢复失败事件失败", "task", task.ID, "err", err)
		return
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, domain.Transition{
			TaskID: task.ID,
			To:     domain.TaskFailed,
			Events: []domain.Event{ev},
		})
	}); err != nil {
		logger.Error("标记恢复失败任务失败", "task", task.ID, "err", err)
	}
}
