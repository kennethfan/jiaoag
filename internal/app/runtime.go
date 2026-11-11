package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// ErrSessionBusy 表示会话里已有运行中的任务。v1 不做排队（M6），直接拒绝，
// 客户端应立即告知用户而不是让请求悬着。
var ErrSessionBusy = errors.New("app: 会话已有运行中的任务，请等它结束或先取消")

// RuntimeDeps 是执行任务所需的外部依赖。
//
// Provider 由调用方按会话配置构造：app 不认识配置文件，也不该认识。
type RuntimeDeps struct {
	Provider func(sess domain.Session) (llm.Provider, string, error)
	// Tools 按会话工作区构造工具集：工具绑定具体根目录，不能全局复用。
	Tools         func(ws *workspace.Workspace) (*tools.Registry, error)
	MaxRounds     int
	ContextBudget int
	Logger        *slog.Logger
}

// taskRun 是一个运行中任务的句柄。
type taskRun struct {
	taskID domain.TaskID
	cancel context.CancelFunc
	done   chan struct{}
}

// ConfigureRuntime 打开任务执行。未配置时 SubmitTask 只入队不执行（M1 语义），
// 这让存储层与协议测试不必构造模型依赖。
func (a *App) ConfigureRuntime(deps RuntimeDeps) error {
	switch {
	case deps.Provider == nil:
		return errors.New("app: 缺少供应商构造函数")
	case deps.Tools == nil:
		return errors.New("app: 缺少工具集")
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	a.runtime = &deps
	return nil
}

// currentRuntime 返回执行依赖快照。依赖在 ConfigureRuntime 之后不再变化，
// 因此在锁外使用快照是安全的。
func (a *App) currentRuntime() *RuntimeDeps {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	return a.runtime
}

// checkIdle 做一次便宜的忙检查，让注定被拒的请求不必先读工作区、建供应商。
// 真正的判定在提交前的第二次检查，这里只是提前失败。
func (a *App) checkIdle(sessionID domain.SessionID) error {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if run, ok := a.runs[sessionID]; ok && run != nil {
		return ErrSessionBusy
	}
	return nil
}

// newAgent 按会话配置构造本次任务用的 agent，并顺带算出提示词与扩展指纹。
//
// deps 由调用方在锁外传入：本函数要读工作区、构造供应商（可能读密钥文件），
// 不能占用 runtimeMu。
func (a *App) newAgent(sess domain.Session, deps *RuntimeDeps) (*agent.Agent, string, string, error) {
	ws, err := workspace.New(sess.Workspace)
	if err != nil {
		return nil, "", "", err
	}
	registry, err := deps.Tools(ws)
	if err != nil {
		return nil, "", "", err
	}
	provider, model, err := deps.Provider(sess)
	if err != nil {
		return nil, "", "", err
	}
	ag, err := agent.New(agent.Options{
		Provider:      provider,
		Tools:         registry,
		Workspace:     ws,
		Recorder:      a,
		Model:         model,
		MaxRounds:     deps.MaxRounds,
		ContextBudget: deps.ContextBudget,
		Logger:        deps.Logger,
	})
	if err != nil {
		return nil, "", "", err
	}
	system, err := ag.SystemPrompt()
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256([]byte(registry.Digest() + "|" + system.Hash))
	return ag, system.Hash, hex.EncodeToString(sum[:]), nil
}

// startLocked 启动执行协程。调用方必须持有 runtimeMu。
func (a *App) startLocked(sess domain.Session, task domain.Task, ag *agent.Agent, deps *RuntimeDeps) {
	ctx, cancel := context.WithCancel(a.baseCtx)
	run := &taskRun{taskID: task.ID, cancel: cancel, done: make(chan struct{})}
	a.runs[sess.ID] = run

	go func() {
		defer close(run.done)
		defer cancel()
		defer a.clearRun(sess.ID, task.ID)

		// 取消可能发生在协程启动之前：markRunning 用不随 ctx 取消的 context
		// 写库，否则任务会停在 running 而没有任何终态。
		if err := a.markRunning(context.WithoutCancel(ctx), task); err != nil {
			deps.Logger.Error("标记任务运行失败", "task", task.ID, "err", err)
			// 已经进了数据库的 queued 任务必须有个终态，否则它会永远挂在
			// 会话里，客户端也看不出它已经死了。
			if ferr := a.failTask(context.WithoutCancel(ctx), task,
				fmt.Sprintf("标记任务运行失败：%v", err)); ferr != nil {
				deps.Logger.Error("补写任务终态失败", "task", task.ID, "err", ferr)
			}
			return
		}
		if err := ag.Run(ctx, task); err != nil {
			deps.Logger.Error("任务执行失败", "task", task.ID, "err", err)
		}
	}()
}

// failTask 为还没交给 agent 的任务补一条失败终态。
//
// 只覆盖「协程没能走到 agent.Run」这一小段：agent 接管之后的终态由它自己写。
func (a *App) failTask(ctx context.Context, task domain.Task, message string) error {
	from := task.Status
	if from == "" {
		from = domain.TaskQueued
	}
	ev, err := domain.NewEvent(domain.EventTaskStatus, task.SessionID, task.ID, domain.TaskStatusPayload{
		From:   from,
		To:     domain.TaskFailed,
		Reason: "internal_error",
		Error:  message,
	})
	if err != nil {
		return err
	}
	return a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, domain.Transition{
			TaskID: task.ID,
			To:     domain.TaskFailed,
			Reason: message,
			Events: []domain.Event{ev},
		})
	})
}

func (a *App) clearRun(sessionID domain.SessionID, taskID domain.TaskID) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if run, ok := a.runs[sessionID]; ok && run.taskID == taskID {
		delete(a.runs, sessionID)
	}
}

// markRunning 记录 queued → running，并发布 round 之外的第一条状态事件。
func (a *App) markRunning(ctx context.Context, task domain.Task) error {
	ev, err := domain.NewEvent(domain.EventTaskStatus, task.SessionID, task.ID, domain.TaskStatusPayload{
		From: domain.TaskQueued,
		To:   domain.TaskRunning,
	})
	if err != nil {
		return err
	}
	return a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, domain.Transition{
			TaskID: task.ID,
			To:     domain.TaskRunning,
			Events: []domain.Event{ev},
		})
	})
}

// CancelTask 取消排队中或运行中的任务。已终止的任务是幂等的空操作。
func (a *App) CancelTask(ctx context.Context, sessionID domain.SessionID, taskID domain.TaskID) error {
	a.runtimeMu.Lock()
	run := a.runs[sessionID]
	if run != nil && run.taskID == taskID {
		a.runtimeMu.Unlock()
		// 运行中的任务由 agent 观察 ctx 自行收尾并记录 canceled。
		run.cancel()
		return nil
	}
	a.runtimeMu.Unlock()

	// 之后的读写都不跟随调用方 ctx：客户端常在发完 task.cancel 后立刻断开，
	// 沿用连接 ctx 会让「取消」随连接一起失效，排队中的任务永远停在 queued。
	ctx = context.WithoutCancel(ctx)

	task, err := a.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status.Terminal() {
		return nil
	}

	// 没有本地协程的 running 任务来自上一次进程（v1 不自动续跑）：
	// 直接标 canceled，避免它永远占着会话。
	ev, err := domain.NewEvent(domain.EventTaskStatus, task.SessionID, task.ID, domain.TaskStatusPayload{
		From:   task.Status,
		To:     domain.TaskCanceled,
		Reason: "canceled",
	})
	if err != nil {
		return err
	}
	return a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, domain.Transition{
			TaskID: taskID,
			To:     domain.TaskCanceled,
			Events: []domain.Event{ev},
		})
	})
}

// Record 实现 agent.Recorder：一次原子迁移（落库 + 广播）。
func (a *App) Record(ctx context.Context, tr domain.Transition) error {
	return a.commit(func() ([]domain.Event, error) {
		return a.store.Apply(ctx, tr)
	})
}

// Events 实现 agent.Recorder：读取会话历史用于重建上下文。
//
// 超过 maxEvents 时保留**最新**的一段。历史是向前读的，丢最新会把用户本次
// 提问（task.queued）一起丢掉，模型就看不到要回答什么了。
func (a *App) Events(ctx context.Context, sessionID domain.SessionID) ([]domain.Event, error) {
	const (
		batch     = 500
		maxEvents = 20000
	)
	var out []domain.Event
	var from domain.Seq
	for {
		chunk, err := a.store.Replay(ctx, sessionID, from, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(chunk) < batch {
			break
		}
		// 一边读一边裁，避免长会话把整段历史都留在内存里。
		if len(out) > 2*maxEvents {
			out = tailEvents(out, maxEvents)
		}
		from = chunk[len(chunk)-1].Seq
	}
	return tailEvents(out, maxEvents), nil
}

// tailEvents 保留最后 max 条事件，并把起点对齐到 task.queued。
//
// 对齐是必需的：一条孤立的 assistant.message（它的提问已被丢掉）会让重建出
// 的消息序列以助手开头，供应商会直接拒绝这种历史。
func tailEvents(events []domain.Event, max int) []domain.Event {
	if max <= 0 || len(events) <= max {
		return events
	}
	start := len(events) - max
	for start < len(events) && events[start].Type != domain.EventTaskQueued {
		start++
	}
	if start >= len(events) {
		// 窗口里没有提问边界（单任务事件就超过上限）：只能硬切，历史可能以
		// assistant.message/tool.result 开头；仅极端会话会走到这里。
		start = len(events) - max
	}
	return events[start:]
}

// Delta 实现 agent.Recorder：临时帧只扇出、不落库，Seq 保持 0。
func (a *App) Delta(ctx context.Context, ev domain.Event) error {
	if ev.Seq != 0 {
		return fmt.Errorf("app: 增量帧不应带 seq（收到 %d）", ev.Seq)
	}
	a.hub.Publish([]domain.Event{ev})
	return nil
}

// waitRuns 等待所有运行中的任务收尾。
func (a *App) waitRuns(timeout time.Duration) {
	a.runtimeMu.Lock()
	chans := make([]chan struct{}, 0, len(a.runs))
	for _, run := range a.runs {
		chans = append(chans, run.done)
	}
	a.runtimeMu.Unlock()

	deadline := time.After(timeout)
	for _, done := range chans {
		select {
		case <-done:
		case <-deadline:
			return
		}
	}
}
