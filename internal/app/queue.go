package app

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
)

// pendingTask 是一个已落库、尚未启动的排队任务。
//
// ag 在提交时就构造好：SystemPromptHash 与 ExtensionsDigest 也是提交时冻结的，
// 排队期间配置变化不能改变这个任务的上下文指纹。
type pendingTask struct {
	sess domain.Session
	task domain.Task
	ag   *agent.Agent
	deps *RuntimeDeps
}

// enqueueLocked 把任务追加到会话队尾。调用方必须持有 runtimeMu。
func (a *App) enqueueLocked(p pendingTask) {
	a.queue[p.sess.ID] = append(a.queue[p.sess.ID], p)
}

// popQueueLocked 弹出队首。队列空时顺手删除键，避免 map 随会话数无限增长。
// 调用方必须持有 runtimeMu。
func (a *App) popQueueLocked(sessionID domain.SessionID) (pendingTask, bool) {
	list := a.queue[sessionID]
	if len(list) == 0 {
		return pendingTask{}, false
	}
	p := list[0]
	if len(list) == 1 {
		delete(a.queue, sessionID)
	} else {
		a.queue[sessionID] = list[1:]
	}
	return p, true
}

// removeQueuedLocked 把指定任务移出排队，返回是否命中。调用方必须持有 runtimeMu。
func (a *App) removeQueuedLocked(sessionID domain.SessionID, taskID domain.TaskID) bool {
	list := a.queue[sessionID]
	if len(list) == 0 {
		return false
	}
	kept := list[:0]
	found := false
	for _, p := range list {
		if p.task.ID == taskID {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		delete(a.queue, sessionID)
	} else {
		a.queue[sessionID] = kept
	}
	return found
}

// startNextLocked 在会话空闲时启动队首。调用方必须持有 runtimeMu。
//
// 这里只做「弹出 + startLocked」，不自己标记 running：标记由执行协程内的
// markRunning 负责。两处都标记会让第二次迁移撞上 queued→running 的非法迁移，
// 协程静默退出，任务永远停在 running。
func (a *App) startNextLocked(sessionID domain.SessionID) {
	if a.closed {
		return
	}
	if a.runs[sessionID] != nil {
		return
	}
	p, ok := a.popQueueLocked(sessionID)
	if !ok {
		return
	}
	a.startLocked(p.sess, p.task, p.ag, p.deps)
}

// cancelQueuedOnClose 在关闭时把「已排队、尚未启动」的任务逐个标 canceled。
//
// 此时 baseCtx 已取消，写库必须用不随它取消的 context；也不能省这一步：
// 留下 queued 的任务会一直挂在会话里，重启后的启动清理才会补终态。
func (a *App) cancelQueuedOnClose(queued map[domain.SessionID][]pendingTask) {
	logger := a.logger()
	ctx := context.Background()
	for _, list := range queued {
		for _, p := range list {
			ev, err := domain.NewEvent(domain.EventTaskStatus, p.task.SessionID, p.task.ID, domain.TaskStatusPayload{
				From:   domain.TaskQueued,
				To:     domain.TaskCanceled,
				Reason: "shutdown",
				Error:  "服务端关闭，排队任务已取消",
			})
			if err != nil {
				logger.Error("构造排队任务取消事件失败", "task", p.task.ID, "err", err)
				continue
			}
			if err := a.commit(func() ([]domain.Event, error) {
				return a.store.Apply(ctx, domain.Transition{
					TaskID: p.task.ID,
					To:     domain.TaskCanceled,
					Events: []domain.Event{ev},
				})
			}); err != nil {
				// 队列快照之后任务可能已被取消：非法迁移说明它已有终态，跳过即可。
				var te *domain.TransitionError
				if errors.As(err, &te) {
					continue
				}
				logger.Error("取消排队任务失败", "task", p.task.ID, "err", err)
			}
		}
	}
}

// canonicalWorkspace 把工作区路径归一成可比较的形式：软链写法与真实目录是
// 同一个仓库，同仓警告必须能识别出来。
func canonicalWorkspace(path string) string {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	// 目录不存在或不可读时退回字面比较：警告只是提示，不该因此报错。
	return clean
}

// sharedWorkspaceSessions 找出同一工作区里还有活跃任务的其它会话。
//
// 只做提示不阻断：两个会话各改各的文件没问题，但构建缓存、dist/、端口会互踩。
func (a *App) sharedWorkspaceSessions(ctx context.Context, workspace string, exclude domain.SessionID) ([]string, error) {
	active, err := a.store.SessionsWithActiveTasks(ctx)
	if err != nil {
		return nil, err
	}
	canonical := canonicalWorkspace(workspace)
	var shared []string
	for _, sess := range active {
		if sess.ID == exclude {
			continue
		}
		if canonicalWorkspace(sess.Workspace) != canonical {
			continue
		}
		shared = append(shared, string(sess.ID))
	}
	return shared, nil
}
