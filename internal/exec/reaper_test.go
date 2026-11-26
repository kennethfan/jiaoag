//go:build !windows

package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// startSleep 起一个真实的后台 sleep 进程，供 KillVerified 的测试用。
func startSleep(t *testing.T, secs string) *Process {
	t.Helper()
	proc, err := startBackground(Plan{Cwd: t.TempDir()}, Spec{LogPath: t.TempDir() + "/bg.log"},
		[]string{"sh", "-c", "sleep " + secs})
	if err != nil {
		t.Fatalf("启动后台进程失败：%v", err)
	}
	t.Cleanup(func() { _ = proc.Stop() })
	return proc
}

// withoutProcStartTime 把启动时间探测临时替换掉，避免依赖本机 ps。
func withoutProcStartTime(t *testing.T, fn func(pid int) (time.Time, error)) {
	t.Helper()
	old := procStartTime
	procStartTime = fn
	t.Cleanup(func() { procStartTime = old })
}

func TestKillVerifiedKillsMatchingProcess(t *testing.T) {
	proc := startSleep(t, "30")
	withoutProcStartTime(t, func(int) (time.Time, error) { return proc.StartedAt, nil })

	table := NewProcTable(4)
	if err := table.KillVerified(context.Background(), ProcRecord{
		ID:        "call_ok",
		PID:       proc.PID,
		StartedAt: proc.StartedAt,
	}); err != nil {
		t.Fatalf("启动时间匹配时应允许终止：%v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for proc.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if proc.Running() {
		t.Fatal("进程应该已被终止")
	}
}

func TestKillVerifiedRejectsReusedPID(t *testing.T) {
	proc := startSleep(t, "30")
	// pid 存在，但库里的启动时间差了一小时：按 pid 复用处理，必须拒绝。
	withoutProcStartTime(t, func(int) (time.Time, error) {
		return proc.StartedAt.Add(-time.Hour), nil
	})

	table := NewProcTable(4)
	err := table.KillVerified(context.Background(), ProcRecord{
		ID:        "call_reused",
		PID:       proc.PID,
		StartedAt: proc.StartedAt,
	})
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("启动时间不一致时应拒绝，实际：%v", err)
	}
	if !proc.Running() {
		t.Fatal("拒绝终止时不应该杀掉进程")
	}
}

func TestKillVerifiedReportsMissingProcess(t *testing.T) {
	withoutProcStartTime(t, func(int) (time.Time, error) { return time.Time{}, errors.New("no such process") })

	table := NewProcTable(4)
	err := table.KillVerified(context.Background(), ProcRecord{ID: "call_gone", PID: 999999, StartedAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "读不到") {
		t.Fatalf("读不到启动时间时应报错，实际：%v", err)
	}
}

func TestKillVerifiedRejectsEmptyPID(t *testing.T) {
	table := NewProcTable(4)
	if err := table.KillVerified(context.Background(), ProcRecord{ID: "call_no_pid"}); err == nil {
		t.Fatal("没有 pid 时应报错")
	}
}
