//go:build unix

package exec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
)

func backgroundPlan(t *testing.T, argv ...string) execpkg.Plan {
	t.Helper()
	b := execpkg.NewDirect()
	plan, err := b.Prepare(t.Context(), execpkg.Command{
		Argv:    argv,
		Cwd:     t.TempDir(),
		Timeout: time.Minute,
		Env:     []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return plan
}

func TestBackgroundStartsWritesLogAndStops(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bg.log")
	bg, ok := any(execpkg.NewDirect()).(execpkg.Background)
	if !ok {
		t.Fatal("direct 后端应实现 Background")
	}
	p, err := bg.Start(backgroundPlan(t, "sh", "-c", "echo started; sleep 30"), execpkg.Spec{LogPath: logPath})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !p.Running() {
		t.Fatal("刚启动的进程应在运行")
	}
	// 等日志出现内容：写入是异步的，读太快会读到空文件。
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "started") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("2s 内没有看到日志内容：%q", string(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 后 3s 仍未退出")
	case <-timeFunc(p):
	}
	if p.Running() {
		t.Fatal("Stop 后进程仍在运行")
	}
}

// timeFunc 把 Process 的退出通道转成一个只读 channel；测试不依赖
// Wait 的阻塞语义，只需要「退出了」这个信号。
func timeFunc(p *execpkg.Process) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		p.Wait()
		close(ch)
	}()
	return ch
}

func TestBackgroundCreatesLogDir(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nested", "deep", "bg.log")
	bg := any(execpkg.NewDirect()).(execpkg.Background)
	p, err := bg.Start(backgroundPlan(t, "sh", "-c", "exit 0"), execpkg.Spec{LogPath: logPath})
	if err != nil {
		t.Fatalf("日志目录不存在时也应能启动: %v", err)
	}
	p.Wait()
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("日志文件未创建: %v", err)
	}
}

func TestProcTableCapsAndCloses(t *testing.T) {
	table := execpkg.NewProcTable(2)
	bg := any(execpkg.NewDirect()).(execpkg.Background)
	start := func() *execpkg.Process {
		p, err := bg.Start(backgroundPlan(t, "sh", "-c", "sleep 30"), execpkg.Spec{LogPath: filepath.Join(t.TempDir(), "x.log")})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return p
	}
	p1, p2, p3 := start(), start(), start()
	p1.ID, p2.ID, p3.ID = "c1", "c2", "c3"
	if err := table.Add("ses", p1); err != nil {
		t.Fatalf("Add p1: %v", err)
	}
	if err := table.Add("ses", p2); err != nil {
		t.Fatalf("Add p2: %v", err)
	}
	if err := table.Add("ses", p3); err == nil {
		t.Fatal("超过单会话上限应报错")
	}
	_ = p3.Stop()

	found, _, err := table.Stop("ses", p1.ID)
	if err != nil || !found {
		t.Fatalf("Stop: found=%v err=%v", found, err)
	}
	if p1.Running() {
		t.Fatal("Stop 之后 p1 仍在运行")
	}
	if found, _, _ := table.Stop("ses", "nope"); found {
		t.Fatal("未知 id 应返回 false")
	}

	table.CloseAll()
	if p2.Running() {
		t.Fatal("CloseAll 应停掉剩余进程")
	}
}
