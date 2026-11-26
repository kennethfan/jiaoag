package exec

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ProcInfo 是后台进程的只读快照，供工具与界面展示。
type ProcInfo struct {
	ID         string
	Argv       []string
	OutputPath string
	StartedAt  time.Time
	Running    bool
}

// ProcTable 按会话登记后台进程。
//
// 为什么按会话：后台命令的语义是「这个工作区里跑着的服务」，会话就是那个
// 工作区的身份。会话删除、服务端关闭时按会话整体收掉，不会误杀别人的进程。
// 表只存指针，进程退出后仍保留（带 Running=false），这样 stop 是幂等的、
// 日志也还能读；超上限时优先清理已退出的条目。
type ProcTable struct {
	mu       sync.Mutex
	max      int
	sessions map[string]map[string]*Process

	// store 是跨运行时的登记与停止意图通道，由装配层注入；nil 表示纯进程内。
	store   ProcStore
	owner   string
	watched map[string]bool
}

// ProcRecord 是后台进程在共享存储里的投影。
type ProcRecord struct {
	ID         string
	SessionID  string
	Owner      string
	OutputPath string
	Argv       []string
	PID        int
	StartedAt  time.Time
	Status     string
}

// ProcStore 让后台进程跨运行时可见、可停止。exec 不依赖存储实现，
// 由 boot 注入（store 适配器负责落表）。
type ProcStore interface {
	RegisterProc(ctx context.Context, rec ProcRecord) error
	MarkProc(ctx context.Context, id, status string) error
	ListProcs(ctx context.Context, sessionID string) ([]ProcRecord, error)
	RequestStop(ctx context.Context, sessionID, id string) error
	StopRequested(ctx context.Context, id string) (bool, error)
	DeleteProc(ctx context.Context, id string) error
}

// 后台进程状态，和 store 包保持一致。
const (
	ProcRunning  = "running"
	ProcExited   = "exited"
	ProcStopped  = "stopped"
	ProcOrphaned = "orphaned"
)

// NewProcTable 创建进程表。max 是单会话并发上限，<=0 时取 4。
func NewProcTable(max int) *ProcTable {
	if max <= 0 {
		max = 4
	}
	return &ProcTable{max: max, sessions: make(map[string]map[string]*Process)}
}

// Max 返回单会话上限。
func (t *ProcTable) Max() int {
	if t == nil {
		return 0
	}
	return t.max
}

// Add 登记一个刚启动的进程，超出上限时调用方必须自行停掉进程。
func (t *ProcTable) Add(sessionID string, p *Process) error {
	return t.add(sessionID, p)
}

func (t *ProcTable) add(sessionID string, p *Process) error {
	if t == nil {
		return fmt.Errorf("exec: 后台进程表不可用")
	}
	if p == nil {
		return fmt.Errorf("exec: 后台进程为空")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = make(map[string]map[string]*Process)
	}
	list := t.sessions[sessionID]
	if list == nil {
		list = make(map[string]*Process)
		t.sessions[sessionID] = list
	}
	// 上限只算还在跑的：已退出的条目留着让日志与 stop 仍可用，但超过
	// max*4 时按启动时间清掉最老的，避免一个长会话把表撑大。
	running := 0
	for _, old := range list {
		if old.Running() {
			running++
		}
	}
	if running >= t.max {
		return fmt.Errorf("exec: 本会话后台进程已达上限 %d，请先用 stop_command 停止", t.max)
	}
	if len(list) >= t.max*4 {
		pruneOldestExited(list)
	}
	list[p.ID] = p
	store, owner := t.store, t.owner
	if store != nil {
		// 登记失败只影响可观测性，不该让已经跑起来的进程失去句柄，
		// 所以异步做、错误忽略。
		rec := ProcRecord{
			ID:         p.ID,
			SessionID:  sessionID,
			Owner:      owner,
			OutputPath: p.OutputPath,
			Argv:       p.Argv,
			PID:        p.PID,
			StartedAt:  p.StartedAt,
			Status:     ProcRunning,
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = store.RegisterProc(ctx, rec)
		}()
	}
	return nil
}

// Attach 注入共享存储并启动「停止意图」轮询。ctx 结束即停止轮询。
func (t *ProcTable) Attach(store ProcStore, owner string, ctx context.Context) {
	if t == nil || store == nil {
		return
	}
	t.mu.Lock()
	t.store = store
	t.owner = owner
	t.watched = make(map[string]bool)
	t.mu.Unlock()
	go t.watchStop(ctx)
}

// watchStop 每秒检查两件事：别的运行时投递的停止意图，以及本地进程退出后
// 把状态回写（否则库里会永远显示 running）。
func (t *ProcTable) watchStop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		type entry struct {
			id      string
			running bool
		}
		t.mu.Lock()
		store, owner := t.store, t.owner
		var entries []entry
		for _, list := range t.sessions {
			for id, p := range list {
				entries = append(entries, entry{id: id, running: p.Running()})
			}
		}
		marked := make(map[string]bool, len(t.watched))
		for id := range t.watched {
			marked[id] = true
		}
		t.mu.Unlock()
		if store == nil {
			continue
		}
		_ = owner
		for _, e := range entries {
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if e.running {
				if stop, err := store.StopRequested(callCtx, e.id); err == nil && stop {
					if p, ok := t.getByID(e.id); ok {
						_ = p.Stop()
					}
					_ = store.MarkProc(callCtx, e.id, ProcStopped)
					t.markWatched(e.id)
				}
			} else if !marked[e.id] {
				_ = store.MarkProc(callCtx, e.id, ProcExited)
				t.markWatched(e.id)
			}
			cancel()
		}
	}
}

func (t *ProcTable) markWatched(id string) {
	t.mu.Lock()
	if t.watched != nil {
		t.watched[id] = true
	}
	t.mu.Unlock()
}

func (t *ProcTable) getByID(id string) (*Process, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, list := range t.sessions {
		if p, ok := list[id]; ok {
			return p, true
		}
	}
	return nil, false
}

// pruneOldestExited 删掉一条已退出且最早启动的条目。
func pruneOldestExited(list map[string]*Process) {
	var oldest *Process
	for _, p := range list {
		if p.Running() {
			continue
		}
		if oldest == nil || p.StartedAt.Before(oldest.StartedAt) {
			oldest = p
		}
	}
	if oldest != nil {
		delete(list, oldest.ID)
	}
}

// Get 按会话与 id 查进程。
func (t *ProcTable) Get(sessionID, id string) (*Process, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.sessions[sessionID][id]
	return p, ok
}

// List 返回会话可见的后台进程，按启动时间排序。
func (t *ProcTable) List(sessionID string) []ProcInfo {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.sessions[sessionID]
	out := make([]ProcInfo, 0, len(list))
	for _, p := range list {
		out = append(out, ProcInfo{
			ID:         p.ID,
			Argv:       append([]string(nil), p.Argv...),
			OutputPath: p.OutputPath,
			StartedAt:  p.StartedAt,
			Running:    p.Running(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Stop 停止指定后台进程。返回 false 表示该会话没有这个 id。
// Stop 返回 (found, local, err)。local 为假表示进程属于别的运行时：
// 这里只能投递 stop_requested，由属主在 1 秒内真正杀掉。
func (t *ProcTable) Stop(sessionID, id string) (bool, bool, error) {
	p, ok := t.Get(sessionID, id)
	if !ok {
		// 进程可能是别的运行时启动的：只能投递停止意图。
		t.mu.Lock()
		store := t.store
		t.mu.Unlock()
		if store == nil {
			return false, false, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		procs, err := store.ListProcs(ctx, sessionID)
		if err != nil {
			return false, false, err
		}
		for _, rec := range procs {
			if rec.ID == id {
				if rec.Status != ProcRunning {
					return true, false, nil
				}
				return true, false, store.RequestStop(ctx, sessionID, id)
			}
		}
		return false, false, nil
	}
	if err := p.Stop(); err != nil {
		return true, true, err
	}
	t.markStopped(p.ID)
	// 等一小段：SIGKILL 后通常立即退出，等不到也不阻塞调用方。
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
	return true, true, nil
}

// StopSession 停掉一个会话的全部后台进程（会话删除时调用）。
func (t *ProcTable) StopSession(sessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	list := t.sessions[sessionID]
	delete(t.sessions, sessionID)
	store := t.store
	t.mu.Unlock()
	stopAll(list)
	if store == nil {
		return
	}
	// 别的运行时启动的进程只能请求停止；本地已杀的直接标 stopped。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	procs, err := store.ListProcs(ctx, sessionID)
	if err != nil {
		return
	}
	for _, rec := range procs {
		if rec.Status != ProcRunning {
			continue
		}
		if _, local := list[rec.ID]; local {
			_ = store.MarkProc(ctx, rec.ID, ProcStopped)
			continue
		}
		_ = store.RequestStop(ctx, sessionID, rec.ID)
	}
}

func (t *ProcTable) markStopped(id string) {
	t.mu.Lock()
	store := t.store
	t.mu.Unlock()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = store.MarkProc(ctx, id, ProcStopped)
}

// CloseAll 停掉所有会话的后台进程（服务端关闭 / 空闲退出）。
func (t *ProcTable) CloseAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	sessions := t.sessions
	t.sessions = make(map[string]map[string]*Process)
	t.mu.Unlock()
	for _, list := range sessions {
		stopAll(list)
	}
}

func stopAll(list map[string]*Process) {
	var wg sync.WaitGroup
	for _, p := range list {
		if p == nil || !p.Running() {
			continue
		}
		wg.Add(1)
		go func(p *Process) {
			defer wg.Done()
			_ = p.Stop()
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
			}
		}(p)
	}
	wg.Wait()
}

// ListStored 返回库里登记的全部后台进程（可按会话过滤），供 /ps 面板使用。
func (t *ProcTable) ListStored(ctx context.Context, sessionID string) ([]ProcRecord, error) {
	if t == nil || t.store == nil {
		return nil, fmt.Errorf("exec: 后台进程存储不可用")
	}
	return t.store.ListProcs(ctx, sessionID)
}

// DropRecord 删掉库里的记录并丢掉本地句柄。running 的进程由存储层拒绝删除。
func (t *ProcTable) DropRecord(ctx context.Context, id string) error {
	if t == nil || t.store == nil {
		return fmt.Errorf("exec: 后台进程存储不可用")
	}
	if err := t.store.DeleteProc(ctx, id); err != nil {
		return err
	}
	t.dropHandle(id)
	return nil
}

func (t *ProcTable) dropHandle(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, list := range t.sessions {
		delete(list, id)
	}
	if t.watched != nil {
		delete(t.watched, id)
	}
}
