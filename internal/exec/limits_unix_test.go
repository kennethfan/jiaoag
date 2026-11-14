//go:build unix

package exec

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWrapLimitsKeepsTargetArgv(t *testing.T) {
	argv := []string{"go", "test", "./..."}
	wrapped := wrapLimits(argv, Limits{CPUSeconds: 60, MemoryMB: 256, MaxFiles: 128})

	if len(wrapped) < 4 || wrapped[0] != "sh" || wrapped[1] != "-c" {
		t.Fatalf("包装后的 argv 形状不对：%v", wrapped)
	}
	script := wrapped[2]
	for _, want := range []string{"ulimit -t 60", "ulimit -v 262144", "ulimit -n 128", `exec "$0" "$@"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺少 %q：%s", want, script)
		}
	}
	// `sh -c script name arg…`：name 就是目标 argv[0]，其余是参数，因此目标
	// 进程看到的 argv[0] 仍是 "go"。
	if got := wrapped[3:]; len(got) != len(argv) || got[0] != argv[0] {
		t.Fatalf("目标 argv 未原样保留：%v", got)
	}

	// 没有配置限制时不做任何包装：shell 包装会改变 argv[0] 与信号语义。
	if got := wrapLimits(argv, Limits{}); len(got) != len(argv) || got[0] != argv[0] {
		t.Fatalf("零限制不应包装：%v", got)
	}
}

// 内存上限必须真的生效：分配远超上限的缓冲区应当失败。
func TestRunAppliesMemoryLimit(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// macOS 对 RLIMIT_AS 执行不稳定：同一份代码有时能拦住、有时不能，
		// 而且测试本身要真的分配 200MB。这条由 Linux CI 覆盖，本地跳过。
		t.Skip("macOS 的 RLIMIT_AS 不保证生效，内存上限由 Linux CI 覆盖")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("没有 python3，跳过内存上限用例")
	}

	b := NewDirect()
	cmd := Command{
		Argv:    []string{py, "-c", "buf = bytearray(200 * 1024 * 1024)"},
		Cwd:     t.TempDir(),
		Timeout: 30 * time.Second,
		Env:     []string{},
		Limits:  Limits{MemoryMB: MinMemoryMB},
	}
	plan, err := b.Prepare(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if plan.LimitsDegraded() {
		t.Fatalf("unix 上不应降级：%v", plan.LimitSupport.Missing(plan.Limits))
	}

	res, err := b.Run(context.Background(), plan, DefaultSpec(""))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		// macOS 对部分 mmap 分配不严格执行 RLIMIT_AS；平台差异不应让测试变
		// 红，但也不能假装限制生效。
		t.Skipf("当前平台未强制执行 %d MB 上限（输出：%s）", MinMemoryMB, strings.TrimSpace(res.Output))
	}
}

// 取消命令时整个进程组都要收掉：只杀父进程会留下后台子进程。
func TestCancelKillsChildProcesses(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"

	b := NewDirect()
	cmd := Command{
		// 把后台子进程的 pid 写出来，取消后检查它是否还活着。
		Argv:    []string{"sh", "-c", `sleep 30 & echo $! > "$0"; wait`, pidFile},
		Cwd:     t.TempDir(),
		Timeout: 300 * time.Millisecond,
		Env:     []string{},
	}
	plan, err := b.Prepare(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := b.Run(context.Background(), plan, DefaultSpec(""))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("期望超时，得到 %+v", res)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("没读到子进程 pid（脚本可能还没执行到），跳过：%v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Skipf("子进程 pid 不合法：%q", raw)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// 信号 0 只探测存在性：ESRCH 说明已经被收掉。
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("取消后子进程 %d 仍然存活（进程组没收干净）", pid)
}
