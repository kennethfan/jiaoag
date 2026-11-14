//go:build unix

package exec_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
)

func run(t *testing.T, argv []string, timeout time.Duration, spec execpkg.Spec) execpkg.Result {
	t.Helper()
	b := execpkg.NewDirect()
	plan, err := b.Prepare(context.Background(), execpkg.Command{
		Argv:    argv,
		Cwd:     t.TempDir(),
		Timeout: timeout,
		Env:     []string{},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := b.Run(context.Background(), plan, spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestDirectRunsCommandAndCapturesOutput(t *testing.T) {
	res := run(t, []string{"/bin/sh", "-c", "echo hello; echo err >&2"}, 5*time.Second, execpkg.DefaultSpec(""))
	if res.ExitCode != 0 {
		t.Fatalf("退出码 = %d", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") || !strings.Contains(res.Output, "err") {
		t.Fatalf("输出未合并：%q", res.Output)
	}
	if res.TimedOut {
		t.Fatal("不应超时")
	}
}

func TestDirectReportsNonZeroExit(t *testing.T) {
	res := run(t, []string{"/bin/sh", "-c", "exit 3"}, 5*time.Second, execpkg.DefaultSpec(""))
	if res.ExitCode != 3 {
		t.Fatalf("退出码 = %d，期望 3", res.ExitCode)
	}
}

func TestDirectTimeoutKillsProcessGroup(t *testing.T) {
	started := time.Now()
	// 派生一个子进程再 wait：只杀父进程会留下孤儿，测试会卡住。
	res := run(t, []string{"/bin/sh", "-c", "sleep 30 & wait"}, 80*time.Millisecond, execpkg.DefaultSpec(""))
	if !res.TimedOut {
		t.Fatalf("应标记超时：%+v", res)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("超时后进程组未被清理，耗时 %s", elapsed)
	}
}

func TestCaptureWritesFullLogAndCapsEvent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "call.log")
	body := strings.Repeat("x", 5000)
	res := run(t, []string{"/bin/sh", "-c", "printf '%s' " + body}, 5*time.Second,
		execpkg.SpecFor(logPath, 400, 1<<20))

	if !res.Truncated {
		t.Fatal("超过事件上限应标记截断")
	}
	if len(res.Output) > 400+64 {
		t.Fatalf("事件载荷 %d 字节，超出上限 400（含提示行余量 64）", len(res.Output))
	}
	if res.OutputPath != logPath {
		t.Fatalf("OutputPath = %q", res.OutputPath)
	}
	stored, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取日志: %v", err)
	}
	if len(stored) != len(body) {
		t.Fatalf("日志长度 = %d，期望 %d", len(stored), len(body))
	}
}

func TestCaptureTruncationKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("中文日志", 2000)
	res := run(t, []string{"/bin/sh", "-c", "printf '%s' " + body}, 5*time.Second,
		execpkg.SpecFor("", 401, 1<<20))
	if !res.Truncated {
		t.Fatal("应标记截断")
	}
	if !utf8.ValidString(res.Output) {
		t.Fatal("截断后不是合法 UTF-8")
	}
}

func TestDirectRequiresExplicitEnv(t *testing.T) {
	b := execpkg.NewDirect()
	if _, err := b.Prepare(context.Background(), execpkg.Command{
		Argv:    []string{"true"},
		Cwd:     t.TempDir(),
		Timeout: time.Second,
	}); err == nil {
		t.Fatal("nil env 应被拒绝：它会继承父进程环境")
	}
}

func TestCaptureTailOnlyKeepsEnd(t *testing.T) {
	body := "printf HEADMARK; yes x | head -c 5000; printf TAILMARK"
	spec := execpkg.SpecFor("", 64, 1<<20)
	spec.TailOnly = true
	res := run(t, []string{"/bin/sh", "-c", body}, 5*time.Second, spec)
	if !res.Truncated {
		t.Fatal("应标记截断")
	}
	if !strings.Contains(res.Output, "TAILMARK") {
		t.Fatalf("尾部标记丢失：%q", res.Output)
	}
	if strings.Contains(res.Output, "HEADMARK") {
		t.Fatalf("tail-only 不应保留开头：%q", res.Output)
	}
}

// check_command 的尾部诊断取合并流：错误写在 stderr 时也必须留在尾部。
// 这条测试守着「不做 stderr 优先」这个决定：合并流不会漏掉任一侧的诊断。
func TestCaptureTailOnlyKeepsStderr(t *testing.T) {
	body := "printf HEADMARK; yes x | head -c 5000; printf ERRMARK >&2"
	spec := execpkg.SpecFor("", 64, 1<<20)
	spec.TailOnly = true
	res := run(t, []string{"/bin/sh", "-c", body}, 5*time.Second, spec)
	if !res.Truncated {
		t.Fatal("应标记截断")
	}
	if !strings.Contains(res.Output, "ERRMARK") {
		t.Fatalf("stderr 诊断丢失：%q", res.Output)
	}
	if strings.Contains(res.Output, "HEADMARK") {
		t.Fatalf("tail-only 不应保留开头：%q", res.Output)
	}
}

// 日志写满与事件截断是两件事：事件预算充足时，事件载荷完整，truncated
// 必须为 false，只有 LogTruncated 记录日志后半段被丢弃。
func TestCaptureLogCapDoesNotMarkEventTruncated(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "big.log")
	res := run(t, []string{"/bin/sh", "-c", "yes | head -c 200000"}, 5*time.Second,
		execpkg.SpecFor(logPath, 1<<20, 1024))
	if res.Truncated {
		t.Fatal("事件载荷完整时不应标记事件截断")
	}
	if !res.LogTruncated {
		t.Fatal("日志超过上限应标记 LogTruncated")
	}
	if strings.Contains(res.Output, "已截断") {
		t.Fatalf("事件输出不该出现截断提示：%q", res.Output[:80])
	}
}

func TestCaptureDiscardsBeyondLogCapButDrains(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "big.log")
	// 4 MB 日志上限在测试里缩小到 1 KB；drain 不做的话子进程会阻塞。
	res := run(t, []string{"/bin/sh", "-c", "yes | head -c 200000"}, 5*time.Second,
		execpkg.SpecFor(logPath, 1024, 1024))
	if res.ExitCode != 0 {
		t.Fatalf("退出码 = %d", res.ExitCode)
	}
	// 200 KB 输出既超过 1 KB 日志上限，也超过 1 KB 事件预算。
	if !res.Truncated || !res.LogTruncated {
		t.Fatalf("截断标志 = event:%v log:%v，期望都为 true", res.Truncated, res.LogTruncated)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1024+256 {
		t.Fatalf("日志超过上限：%d 字节", info.Size())
	}
}
