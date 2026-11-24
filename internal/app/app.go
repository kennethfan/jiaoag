// Package app 是应用层：唯一的事务边界与唯一的事件发布者。
//
// M1 覆盖「账本」：会话与任务的创建、状态迁移落库、事件回放。M3 在本层
// 接上任务执行：app 负责协程生命周期与取消。M6 起会话内任务按 FIFO 排队，
// 跨会话并行；agent 只负责一轮轮对话。执行依赖通过 ConfigureRuntime 注入，
// 未注入时退回只入队的行为。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/agent"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	workspacepkg "github.com/kennethfan/jiaoag/internal/workspace"
)

// App 组合持久化与事件扇出。除它之外没有第二个写者。
type App struct {
	store Store
	hub   *event.Hub
	// notify 是可选的旁路回调（webhook 通知）；在 commit 内同步调用，
	// 实现必须自身异步，不得阻塞。
	notify func(domain.Event)

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
	// maxQueueDepth 是单会话排队任务数上限；0 表示不限。判定在提交的锁内完成，
	// 因此排队上限与「落库」是原子的：被拒的任务不会留下一条多余的 queued。
	maxQueueDepth int
	// slots 是跨会话的并发槽，容量即同时执行的任务数上限；nil 表示不限。
	// 槽只在 startLocked 里获取，拿不到就把任务放回队首，等有人让出槽再拉起。
	slots chan struct{}
	// sessionApprovalTTL 是 allow_session 授权的有效期，受 runtimeMu 保护。
	// 它只影响新写入的授权，已有记录按各自的 expires_at 生效。
	sessionApprovalTTL time.Duration
	// closed 表示 Close 已经开始：之后提交的任务一律拒绝。它与「接受提交」
	// 共用一把锁，因此不存在「已经通过忙检查、在关库之后才落库」的提交。
	closed bool
	// resumeQueued 表示重启后继续执行库里的 queued 任务：关闭时不再把它们
	// 标成 canceled，启动收尾也跳过它们。
	resumeQueued bool

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
	// 通知在落库与广播之后触发：投递失败只该少一条提醒，不该影响任务。
	if a.notify != nil {
		for _, ev := range stored {
			a.notify(ev)
		}
	}
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

// HasBusyTasks 报告是否还有排队或执行中的任务。空闲退出用它判断现在关
// 服务端会不会打断工作。
//
// 等待审批的任务不算：客户端已经全部断开，没有人能裁决它，再等下去只会
// 让服务端永不退出。执行中的任务算，等它跑完再走。
func (a *App) HasBusyTasks(ctx context.Context) (bool, error) {
	tasks, err := a.store.UnfinishedTasks(ctx)
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		if task.Status == domain.TaskQueued || task.Status == domain.TaskRunning {
			return true, nil
		}
	}
	return false, nil
}

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
	// 队列里的任务还没起协程：默认必须在关库前标掉，不能留给下个进程的
	// 启动清理；开启续跑时它们保持 queued，由下次启动重建队列。
	queued := a.queue
	resumeQueued := a.resumeQueued
	a.queue = make(map[domain.SessionID][]pendingTask)
	a.runtimeMu.Unlock()

	if a.baseCancel != nil {
		a.baseCancel()
	}
	if !resumeQueued {
		a.cancelQueuedOnClose(queued)
	}
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

// ErrQueueFull 表示该会话的排队任务已达配置上限，客户端应稍后重试。
var ErrQueueFull = errors.New("app: 会话排队任务已满，请等前面的任务结束")

// Hub 暴露扇出器给传输层（订阅在线事件）。
func (a *App) Hub() *event.Hub { return a.hub }

// NewID 生成带前缀的随机标识，例如 ses_1f3c…。
func NewID(prefix string) (string, error) { return domain.NewID(prefix) }

// UpdateSession 切换会话后续任务使用的供应商与模型。
//
// 只影响新建的 agent；正在执行的任务按提交时冻结的配置跑完。
func (a *App) UpdateSession(ctx context.Context, id domain.SessionID, provider, model string) (domain.Session, error) {
	if a.closedNow() {
		return domain.Session{}, ErrAppClosed
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if model == "" {
		return domain.Session{}, fmt.Errorf("模型不能为空")
	}
	sess, err := a.store.GetSession(ctx, id)
	if err != nil {
		return domain.Session{}, err
	}
	if provider == "" {
		provider = sess.Provider
	}
	sess.Provider = provider
	sess.Model = model
	sess.UpdatedAt = domain.Now()

	ev, err := domain.NewEvent(domain.EventSessionUpdated, sess.ID, "",
		domain.SessionUpdatedPayload{Provider: provider, Model: model})
	if err != nil {
		return domain.Session{}, err
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.UpdateSession(ctx, sess, []domain.Event{ev})
	}); err != nil {
		return domain.Session{}, err
	}
	return sess, nil
}

// SessionSummaries 是会话管理面板的数据源。
//
// workspace 为空表示列出全部。计数由存储层一次算出：面板要的是「哪条该留」，
// 而不是逐条再查一遍任务。
func (a *App) SessionSummaries(ctx context.Context, workspace string) ([]domain.SessionSummary, error) {
	// 空工作区表示「全列」；否则在 Go 里按规范化路径比对。
	//
	// 不在 SQL 里直接比字符串：历史会话可能存的是 /tmp/x，而命令行传的是
	// /private/tmp/x（macOS 上 /tmp 是符号链接）。会话总量是几十条量级，
	// 内存过滤比让用户猜「该写哪个路径」划算。
	summaries, err := a.store.ListSessionSummaries(ctx, "")
	if err != nil {
		return nil, err
	}
	if workspace == "" {
		return summaries, nil
	}
	if abs, err := filepath.Abs(workspace); err == nil {
		workspace = abs
	}
	want := canonicalWorkspace(workspace)
	filtered := make([]domain.SessionSummary, 0, len(summaries))
	for _, sum := range summaries {
		if canonicalWorkspace(sum.Workspace) == want {
			filtered = append(filtered, sum)
		}
	}
	return filtered, nil
}

// RenameSession 改会话标题；空标题表示清掉。
func (a *App) RenameSession(ctx context.Context, id domain.SessionID, title string) (domain.Session, error) {
	if a.closedNow() {
		return domain.Session{}, ErrAppClosed
	}
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > maxSessionTitleRunes {
		return domain.Session{}, fmt.Errorf("标题最多 %d 个字符", maxSessionTitleRunes)
	}
	sess, err := a.store.GetSession(ctx, id)
	if err != nil {
		return domain.Session{}, err
	}
	if sess.Title == title {
		return sess, nil
	}
	sess.Title = title
	sess.UpdatedAt = domain.Now()

	ev, err := domain.NewEvent(domain.EventSessionRenamed, sess.ID, "",
		domain.SessionRenamedPayload{Title: title})
	if err != nil {
		return domain.Session{}, err
	}
	if err := a.commit(func() ([]domain.Event, error) {
		return a.store.RenameSession(ctx, sess, []domain.Event{ev})
	}); err != nil {
		return domain.Session{}, err
	}
	return sess, nil
}

// maxSessionTitleRunes 是标题上限：它只进状态栏与面板，太长会把两者都挤爆。
const maxSessionTitleRunes = 60

// DeleteSession 永久删除会话与关联数据。
//
// 有未终态任务时存储层返回 domain.ErrSessionBusy：那种时候删掉会话，正在跑的
// agent 就失去归属了。调用方应让用户先取消任务或等它结束。
func (a *App) DeleteSession(ctx context.Context, id domain.SessionID) error {
	if a.closedNow() {
		return ErrAppClosed
	}
	return a.store.DeleteSession(ctx, id)
}

// CreateSession 创建工作区会话，并写入 session.created。
func (a *App) CreateSession(ctx context.Context, workspacePath, provider, model, title string, roots ...domain.SessionRoot) (domain.Session, error) {
	// 关闭之后 store 马上就要关：这里直接拒绝，不让调用方拿 sqlite 的
	// 「数据库已关闭」当成业务错误。
	if a.closedNow() {
		return domain.Session{}, ErrAppClosed
	}
	if !filepath.IsAbs(workspacePath) {
		return domain.Session{}, fmt.Errorf("app: 工作区必须是绝对路径: %q", workspacePath)
	}
	primary, err := workspacepkg.New(workspacePath)
	if err != nil {
		return domain.Session{}, err
	}
	extras := make([]workspacepkg.NamedRoot, 0, len(roots))
	for _, r := range roots {
		extras = append(extras, workspacepkg.NamedRoot{Alias: r.Alias, Path: r.Path})
	}
	set, err := workspacepkg.NewRoots(primary, extras)
	if err != nil {
		return domain.Session{}, err
	}
	id, err := NewID("ses")
	if err != nil {
		return domain.Session{}, err
	}
	now := domain.Now()
	sess := domain.Session{
		ID:        domain.SessionID(id),
		Workspace: filepath.Clean(workspacePath),
		Provider:  strings.TrimSpace(provider),
		Model:     strings.TrimSpace(model),
		Title:     strings.TrimSpace(title),
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, r := range set.Extra() {
		sess.Roots = append(sess.Roots, domain.SessionRoot{Alias: r.Alias, Path: r.Path})
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
		Roots:           sess.Roots,
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

// maxTaskSkills 限制单次任务能点名的技能数：每份正文最多 96 KB，
// 再多就把上下文吃光了，通常是用户点错了。
const maxTaskSkills = 4

// normalizeSkills 去空白、去重、限数量。
func normalizeSkills(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > maxTaskSkills {
		return nil, fmt.Errorf("app: 一次最多指定 %d 个技能", maxTaskSkills)
	}
	return out, nil
}

// SubmitOptions 是提交任务时的单次覆盖，只影响这一个任务。
//
// Provider/Model 空表示跟随会话：用户没有点名时，后续改会话模型仍然生效。
type SubmitOptions struct {
	Skills   []string
	Provider string
	Model    string
}

// SubmitTask 提交只带技能的普通任务（兼容既有调用点）。
func (a *App) SubmitTask(ctx context.Context, sessionID domain.SessionID, prompt string, requested ...string) (domain.Task, error) {
	return a.SubmitTaskWith(ctx, sessionID, prompt, SubmitOptions{Skills: requested})
}

// SubmitTaskWith 提交一个任务：会话空闲就立刻执行，已有任务在跑就排到队尾。
//
// 技能与模型覆盖都会随任务一起入库：重启续跑按任务自己记录的值重建，而不是
// 回落到会话当前设置——否则用户点名用的便宜模型，重启后悄悄换成贵的。
//
// 排队只放内存队列，数据库里的状态仍是 queued（唯一的状态来源）。
func (a *App) SubmitTaskWith(ctx context.Context, sessionID domain.SessionID, prompt string, opts SubmitOptions) (domain.Task, error) {
	if strings.TrimSpace(prompt) == "" {
		return domain.Task{}, fmt.Errorf("app: 任务内容为空")
	}
	wanted, err := normalizeSkills(opts.Skills)
	if err != nil {
		return domain.Task{}, err
	}
	overrideProvider := strings.TrimSpace(opts.Provider)
	overrideModel := strings.TrimSpace(opts.Model)
	// 早退：关闭之后不必再读会话、建供应商。真正的判定在提交前的锁内。
	if a.closedNow() {
		return domain.Task{}, ErrAppClosed
	}
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Task{}, err
	}
	// 单次覆盖只改这份会话副本：provider 闭包按它取供应商与模型，
	// 不改库里的会话，所以下一个任务仍回到会话默认。
	if overrideProvider != "" {
		sess.Provider = overrideProvider
	}
	if overrideModel != "" {
		sess.Model = overrideModel
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
		ag, systemHash, extensionsDigest, err = a.newAgent(sess, deps, wanted)
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
		Skills:           wanted,
		Provider:         overrideProvider,
		Model:            overrideModel,
		SystemPromptHash: systemHash,
		ExtensionsDigest: extensionsDigest,
		CreatedAt:        domain.Now(),
	}
	ev, err := domain.NewEvent(domain.EventTaskQueued, sessionID, task.ID, domain.TaskQueuedPayload{
		Prompt:           task.Prompt,
		Skills:           task.Skills,
		Provider:         task.Provider,
		Model:            task.Model,
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
	// 排队上限在落库前判定：被拒的任务不该留下一条永远排不上的 queued。
	if a.maxQueueDepth > 0 && len(a.queue[sessionID]) >= a.maxQueueDepth {
		return domain.Task{}, ErrQueueFull
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

// PendingApprovalPayloads 返回增量订阅时无法靠回放补齐的待审批载荷。
func (a *App) PendingApprovalPayloads(ctx context.Context, sessionID domain.SessionID) ([]json.RawMessage, error) {
	return a.store.PendingApprovalPayloads(ctx, sessionID)
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
