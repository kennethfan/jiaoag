package exec

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeProcStore 模拟共享库：只关心停止意图与状态回写。
type fakeProcStore struct {
	mu     sync.Mutex
	stop   bool
	marks  map[string]string
	listed []ProcRecord
}

func (f *fakeProcStore) RegisterProc(_ context.Context, rec ProcRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, rec)
	return nil
}

func (f *fakeProcStore) MarkProc(_ context.Context, id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marks == nil {
		f.marks = map[string]string{}
	}
	f.marks[id] = status
	return nil
}

func (f *fakeProcStore) ListProcs(context.Context, string) ([]ProcRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProcRecord(nil), f.listed...), nil
}

func (f *fakeProcStore) RequestStop(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stop = true
	return nil
}

func (f *fakeProcStore) StopRequested(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stop, nil
}

func (f *fakeProcStore) DeleteProc(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return nil
}

func (f *fakeProcStore) setStop() {
	f.mu.Lock()
	f.stop = true
	f.mu.Unlock()
}

func (f *fakeProcStore) marked(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marks[id]
}

// TestStopRequestedByOtherRuntime 验证别的运行时投递的停止意图会被属主执行：
// 进程表轮询到 stop_requested 后杀进程，并把状态回写成 stopped。
func TestStopRequestedByOtherRuntime(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"sh", "-c", "sleep 5"}
	proc, err := startBackground(
		Plan{Argv: argv, Cwd: dir, Env: []string{"PATH=/usr/bin:/bin"}},
		Spec{LogPath: filepath.Join(dir, "bg.log")},
		argv,
	)
	if err != nil {
		t.Fatalf("启动后台进程: %v", err)
	}
	proc.ID = "call_stop"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeProcStore{}
	table := NewProcTable(4)
	table.Attach(store, "rt_owner", ctx)
	if err := table.Add("ses_1", proc); err != nil {
		t.Fatalf("登记: %v", err)
	}
	t.Cleanup(func() { table.CloseAll() })

	store.setStop()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !proc.Running() && store.marked("call_stop") == ProcStopped {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("停止意图未被执行：running=%v mark=%q", proc.Running(), store.marked("call_stop"))
}

// TestLocalStopStaysStopped 验证本进程主动停止后，轮询不会把 stopped 覆盖成
// exited：覆盖了就看不出「这条是被谁停的」。
func TestLocalStopStaysStopped(t *testing.T) {
	dir := t.TempDir()
	argv := []string{"sh", "-c", "sleep 5"}
	proc, err := startBackground(
		Plan{Argv: argv, Cwd: dir, Env: []string{"PATH=/usr/bin:/bin"}},
		Spec{LogPath: filepath.Join(dir, "bg.log")},
		argv,
	)
	if err != nil {
		t.Fatalf("启动后台进程: %v", err)
	}
	proc.ID = "call_local"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeProcStore{}
	table := NewProcTable(4)
	table.Attach(store, "rt_owner", ctx)
	if err := table.Add("ses_1", proc); err != nil {
		t.Fatalf("登记: %v", err)
	}
	t.Cleanup(func() { table.CloseAll() })

	found, local, err := table.Stop("ses_1", "call_local")
	if err != nil || !found || !local {
		t.Fatalf("本地停止失败：found=%v local=%v err=%v", found, local, err)
	}
	// 跨过至少两轮轮询（1s 一次）：第二轮最容易把 stopped 覆盖成 exited。
	time.Sleep(2500 * time.Millisecond)
	if got := store.marked("call_local"); got != ProcStopped {
		t.Fatalf("停止后的状态被改写：want %q got %q", ProcStopped, got)
	}
}
