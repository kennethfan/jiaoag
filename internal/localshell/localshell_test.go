package localshell

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// collect 读到 Done 为止，把输出行与结束态分开返回。
func collect(t *testing.T, run *Run) ([]string, Event) {
	t.Helper()
	var lines []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				t.Fatal("channel 在 Done 之前关闭")
			}
			if ev.Done {
				return lines, ev
			}
			lines = append(lines, ev.Text)
		case <-deadline:
			t.Fatal("等本地命令结束超时")
		}
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("用 sh 语法构造输出")
	}
}

func TestStreamsOutputAndExitCode(t *testing.T) {
	skipOnWindows(t)
	run := Start(Options{Workdir: t.TempDir()}, `printf 'a\nb\n'`)
	lines, done := collect(t, run)

	if done.ExitCode != 0 || done.Err != nil {
		t.Fatalf("结束态 = %+v，期望退出码 0", done)
	}
	if strings.Join(lines, ",") != "a,b" {
		t.Fatalf("输出行 = %v", lines)
	}
}

func TestNonZeroExitIsReported(t *testing.T) {
	skipOnWindows(t)
	run := Start(Options{Workdir: t.TempDir()}, `echo boom >&2; exit 3`)
	_, done := collect(t, run)

	if done.ExitCode != 3 {
		t.Fatalf("退出码 = %d，期望 3", done.ExitCode)
	}
	if done.TimedOut || done.Stopped {
		t.Fatalf("不应标记为超时/终止：%+v", done)
	}
}

func TestWorkdirIsApplied(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	run := Start(Options{Workdir: dir}, "pwd")
	lines, done := collect(t, run)

	if done.ExitCode != 0 {
		t.Fatalf("pwd 退出码 = %d", done.ExitCode)
	}
	// macOS 的临时目录在 /var 下，pwd 可能回 /private/var，用包含关系断言。
	if len(lines) != 1 || !strings.Contains(lines[0], strings.TrimPrefix(dir, "/private")) {
		t.Fatalf("pwd 输出 = %v，期望包含 %s", lines, dir)
	}
}

func TestTimeoutKillsCommand(t *testing.T) {
	skipOnWindows(t)
	start := time.Now()
	run := Start(Options{Workdir: t.TempDir(), Timeout: 100 * time.Millisecond}, "sleep 5")
	_, done := collect(t, run)

	if !done.TimedOut {
		t.Fatalf("期望标记超时：%+v", done)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时没有生效，耗时 %s", elapsed)
	}
}

func TestStopKillsProcessGroup(t *testing.T) {
	skipOnWindows(t)
	run := Start(Options{Workdir: t.TempDir()}, "sleep 5; echo never")
	time.Sleep(50 * time.Millisecond)
	run.Stop()

	lines, done := collect(t, run)
	if !done.Stopped {
		t.Fatalf("期望标记为已终止：%+v", done)
	}
	if len(lines) != 0 {
		t.Fatalf("被杀掉的命令不应再有输出：%v", lines)
	}
}

func TestLongLineIsClipped(t *testing.T) {
	skipOnWindows(t)
	long := strings.Repeat("x", MaxLineRunes+500)
	run := Start(Options{Workdir: t.TempDir()}, `printf '%s\n' "`+long+`"`)
	lines, done := collect(t, run)

	if done.ExitCode != 0 {
		t.Fatalf("退出码 = %d", done.ExitCode)
	}
	if len(lines) != 1 {
		t.Fatalf("输出行数 = %d，期望 1", len(lines))
	}
	if n := len([]rune(lines[0])); n != MaxLineRunes+1 {
		t.Fatalf("裁剪后 rune 数 = %d，期望 %d", n, MaxLineRunes+1)
	}
	if !strings.HasSuffix(lines[0], "…") {
		t.Fatalf("裁剪后应以省略号结尾")
	}
}

func TestEmptyCommandExitsCleanly(t *testing.T) {
	skipOnWindows(t)
	run := Start(Options{Workdir: t.TempDir()}, "")
	_, done := collect(t, run)

	if done.ExitCode != 0 {
		t.Fatalf("空命令退出码 = %d", done.ExitCode)
	}
}
