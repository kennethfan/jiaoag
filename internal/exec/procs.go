package exec

import (
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
}

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
	return nil
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
func (t *ProcTable) Stop(sessionID, id string) (bool, error) {
	p, ok := t.Get(sessionID, id)
	if !ok {
		return false, nil
	}
	if err := p.Stop(); err != nil {
		return true, err
	}
	// 等一小段：SIGKILL 后通常立即退出，等不到也不阻塞调用方。
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
	return true, nil
}

// StopSession 停掉一个会话的全部后台进程（会话删除时调用）。
func (t *ProcTable) StopSession(sessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	list := t.sessions[sessionID]
	delete(t.sessions, sessionID)
	t.mu.Unlock()
	stopAll(list)
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
