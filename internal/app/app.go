// Package app 是应用层：唯一的事务边界与唯一的事件发布者。
//
// M1 覆盖「账本」：会话与任务的创建、状态迁移落库、事件回放。M3 在本层
// 接上任务执行：app 负责协程生命周期与取消。M6 起会话内任务按 FIFO 排队，
// 跨会话并行；agent 只负责一轮轮对话。执行依赖通过 ConfigureRuntime 注入，
// 未注入时退回只入队的行为。
package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kennethfan/jiaoag/internal/agent"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
)

// App 组合持久化与事件扇出。除它之外没有第二个写者。
type App struct {
	store Store
	hub   *event.Hub

	// commitMu 保证「提交顺序 = 广播顺序」。
	//
	// 存储层的写锁在写方法返回时就释放了；若在此之后才广播，两个并发写入者
	// 可能先广播较大的 seq，在线订阅者按 seq 去重时就会丢掉较小的那条。
	// 跨会话共用一把锁是可接受的：存储层本来就是全局写锁，这里不引入新的
	// 串行点。所有写入都必须经由 App.commit，不要绕过它直接写库再广播。
	commitMu sync.Mutex

	// baseCtx 是所有任务的父 context：Close 时统一取消。
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// runtimeMu 保护 runtime、runs、queue 与 closed。它覆盖「查表 + 登记」，
	// 也允许在锁内提交：提交与出队必须原子，否则队首会被拉起两次。
	//
	// 锁顺序固定为 runtimeMu → commitMu，**禁止反向**：在 commit 回调里取
	// runtimeMu 会与这里的嵌套直接死锁。
	runtimeMu sync.Mutex
	runtime   *RuntimeDeps
	runs      map[domain.SessionID]*taskRun
	// queue 是每个会话的 FIFO 队列：只放已经落库的 queued 任务与提交时构造
	// 好的 agent（prompt hash / extensions digest 在提交时就冻结）。
	queue map[domain.SessionID][]pendingTask
	// closed 表示 Close 已经开始：之后提交的任务一律拒绝。它与「接受提交」
	// 共用一把锁，因此不存在「已经通过忙检查、在关库之后才落库」的提交。
	closed bool

	// approvals 保存审批等待者与会话级白名单，见 approval.go。它有自己的锁：
	// 等待裁决是长时间阻塞的，不能占用 runtimeMu（否则取消与提交都会被卡住）。
	approvals approvalState
}

// commit 在 commitMu 内完成「存储提交 → hub 广播」，是应用层唯一的写入入口。
// 任何新增写操作（M2 的 Apply 等）都必须走这里，否则提交顺序会与广播顺序脱钩。
// 对外只返回错误：事件的去向只有 hub 与事件日志，调用方不需要副本。
func (a *App) commit(fn func() ([]domain.Event, error)) error {
	a.commitMu.Lock()
	defer a.commitMu.Unlock()

	stored, err := fn()
	if err != nil {
		return err
	}
	a.hub.Publish(stored)
	return nil
}

func New(store Store, hub *event.Hub) *App {
	if hub == nil {
		hub = event.NewHub()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		store:      store,
		hub:        hub,
		baseCtx:    ctx,
		baseCancel: cancel,
		runs:       make(map[domain.SessionID]*taskRun),
		queue:      make(map[domain.SessionID][]pendingTask),
	}
}

// ErrAppClosed 表示 App 已开始关闭：进程退出路径上不再接受新任务。
var ErrAppClosed = errors.New("app: 正在关闭，不再接受新任务")

// closeGrace 是 Close 留给任务自行收尾的窗口。取消之后命令执行与供应商调用
// 都会立即返回，正常任务在这个窗口内就能写完终态；窗口要比单条命令的超时短，
// 否则退出会白白等待，剩下的由补写终态兜底。
const closeGrace = 5 * time.Second

// Close 取消运行中的任务、断开订阅并关闭存储。
//
// 先取消再等待：任务收到取消会记录 canceled 终态，此时数据库仍然打开；
// 直接关库会让这些写入失败，任务永远停在 running。
func (a *App) Close() error { return a.close(closeGrace) }

// close 是 Close 的实现，窗口可注入：测试用短窗口覆盖「窗口用完仍没收尾」的分支。
func (a *App) close(grace time.Duration) error {
	// 在 runtimeMu 内置位，与 SubmitTask 的登记互斥：置位时要么没有任何
	// 提交在路上，要么那把锁正在被某个提交持有——等它把任务登记、落库、
	// 启动完再置位，waitRuns 的快照就必然包含它。置位之后提交一律被拒，
	// 不会再出现「任务已入队却没人执行、也没有终态」的记录。
	//
	// 重复关闭（defer + 显式调用）直接视为已完成：既不再等一轮窗口，也不会
	// 拿已关的库去补写终态。首次关闭的错误不回放，调用方当时就拿到了。
	a.runtimeMu.Lock()
	if a.closed {
		a.runtimeMu.Unlock()
		return nil
	}
	a.closed = true
	// 队列里的任务还没起协程：必须在关库前标掉，不能留给下个进程的启动清理。
	queued := a.queue
	a.queue = make(map[domain.SessionID][]pendingTask)
	a.runtimeMu.Unlock()

	if a.baseCancel != nil {
		a.baseCancel()
	}
	a.cancelQueuedOnClose(queued)
	// 窗口用完还没收尾的任务必须在这里补写终态：下面的关库之后它们再也写不
	// 进去，库里就会留下一条永远 running 的任务。
	a.abandonRuns(a.waitRuns(grace))
	a.hub.CloseAll()
	return a.store.Close()
}

// closedNow 报告 App 是否已开始关闭。
func (a *App) closedNow() bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	return a.closed
}

// Hub 暴露扇出器给传输层（订阅在线事件）。
func (a *App) Hub() *event.Hub { return a.hub }

// NewID 生成带前缀的随机标识，例如 ses_1f3c…。
func NewID(prefix string) (string, error) { return domain.NewID(prefix) }

// CreateSession 创建工作区会话，并写入 session.created。
func (a *App) CreateSession(ctx context.Context, workspace, provider, model, title string) (domain.Session, error) {
	// 关闭之后 store 马上就要关：这里直接拒绝，不让调用方拿 sqlite 的
	// 「数据库已关闭」当成业务错误。
	if a.closedNow() {
		return domain.Session{}, ErrAppClosed
	}
	if !filepath.IsAbs(workspace) {
		return domain.Session{}, fmt.Errorf("app: 工作区必须是绝对路径: %q", workspace)
	}
	id, err := NewID("ses")
	if err != nil {
		return domain.Session{}, err
	}
	now := domain.Now()
	sess := domain.Session{
		ID:        domain.SessionID(id),
		Workspace: filepath.Clean(workspace),
		Provider:  strings.TrimSpace(provider),
		Model:     strings.TrimSpace(model),
		Title:     strings.TrimSpace(title),
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 同仓只警告不拒绝：两个会话各改各的文件没问题，命令与构建会互踩。
	// 这条查询失败不该挡住建会话——它只是提示。
	shared, err := a.sharedWorkspaceSessions(ctx, sess.Workspace, sess.ID)
	if err != nil {
		a.logger().Warn("检查同一工作区的活跃会话失败", "workspace", sess.Workspace, "err", err)
		shared = nil
	}
	ev, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
		Workspace:       sess.Workspace,
		Provider:        sess.Provider,
		Model:           sess.Model,
		Title:           sess.Title,
		SharedWorkspace: shared,
	})
	if err != nil {
		return domain.Session{}, err
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.CreateSession(ctx, sess, []domain.Event{ev})
	}); err != nil {
		return domain.Session{}, err
	}
	return sess, nil
}

// SubmitTask 提交一个任务：会话空闲就立刻执行，已有任务在跑就排到队尾。
//
// 排队只放内存队列，数据库里的状态仍是 queued（唯一的状态来源）。
func (a *App) SubmitTask(ctx context.Context, sessionID domain.SessionID, prompt string) (domain.Task, error) {
	if strings.TrimSpace(prompt) == "" {
		return domain.Task{}, fmt.Errorf("app: 任务内容为空")
	}
	// 早退：关闭之后不必再读会话、建供应商。真正的判定在提交前的锁内。
	if a.closedNow() {
		return domain.Task{}, ErrAppClosed
	}
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Task{}, err
	}

	// 两次加锁：先快照依赖，再复查登记。
	//
	// newAgent 要读工作区、构造供应商（可能读密钥文件）。把这些 I/O 放在
	// runtimeMu 里会让所有会话的提交与取消一起排队等它；但「任务登记」必须
	// 原子，所以锁外只做构造，锁内再判断该入队还是立刻启动。
	deps := a.currentRuntime()

	var (
		ag               *agent.Agent
		systemHash       string
		extensionsDigest string
	)
	if deps != nil {
		ag, systemHash, extensionsDigest, err = a.newAgent(sess, deps)
		if err != nil {
			return domain.Task{}, err
		}
	}

	id, err := NewID("tsk")
	if err != nil {
		return domain.Task{}, err
	}
	task := domain.Task{
		ID:               domain.TaskID(id),
		SessionID:        sessionID,
		Status:           domain.TaskQueued,
		Prompt:           prompt,
		SystemPromptHash: systemHash,
		ExtensionsDigest: extensionsDigest,
		CreatedAt:        domain.Now(),
	}
	ev, err := domain.NewEvent(domain.EventTaskQueued, sessionID, task.ID, domain.TaskQueuedPayload{
		Prompt:           task.Prompt,
		SystemPromptHash: task.SystemPromptHash,
		ExtensionsDigest: task.ExtensionsDigest,
	})
	if err != nil {
		return domain.Task{}, err
	}
	// 登记与「入队 / 立刻启动」必须在同一把锁里完成：否则两个并发提交都会
	// 看到「没有 run」，各自起一个协程，会话内串行就失了。
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if a.closed {
		return domain.Task{}, ErrAppClosed
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.CreateTask(ctx, task, []domain.Event{ev})
	}); err != nil {
		return domain.Task{}, err
	}
	pending := pendingTask{sess: sess, task: task, ag: ag, deps: deps}
	switch {
	case deps == nil || ag == nil:
		// 未配置执行：只落库不跑（M1 语义，存储与协议测试依赖它）。
	case a.runs[sessionID] != nil:
		// 已有任务在跑：排队，等它让出会话。
		a.enqueueLocked(pending)
	case len(a.queue[sessionID]) > 0:
		// 队列非空却没有 run：队首的启动还没落地（例如刚被 clearRun 拉起）。
		// 新任务必须排到队尾，别插队。
		a.enqueueLocked(pending)
		a.startNextLocked(sessionID)
	default:
		a.startLocked(sess, task, ag, deps)
	}
	return task, nil
}

// Replay 返回从 from 之后的事件，供断线重连补齐。
func (a *App) Replay(ctx context.Context, sessionID domain.SessionID, from domain.Seq, limit int) ([]domain.Event, error) {
	return a.store.Replay(ctx, sessionID, from, limit)
}

func (a *App) LastSeq(ctx context.Context, sessionID domain.SessionID) (domain.Seq, error) {
	return a.store.LastSeq(ctx, sessionID)
}

func (a *App) ListSessions(ctx context.Context) ([]domain.Session, error) {
	return a.store.ListSessions(ctx)
}

func (a *App) GetSession(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	return a.store.GetSession(ctx, id)
}

func (a *App) GetTask(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	return a.store.GetTask(ctx, id)
}

func (a *App) GetToolCall(ctx context.Context, id domain.ToolCallID) (domain.ToolCall, error) {
	return a.store.GetToolCall(ctx, id)
}

func (a *App) ListTasks(ctx context.Context, sessionID domain.SessionID) ([]domain.Task, error) {
	return a.store.ListTasks(ctx, sessionID)
}
