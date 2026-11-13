package exec

import (
	"os"
	"path/filepath"
	"testing"
)

// 日志打不开时命令仍可以跑，但结果必须明确说出「全文没有落盘」：
// 静默丢日志等于对外宣称 output_path 可用，而实际上没有文件。
func TestCaptureReportsLogOpenFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newCapture(SpecFor(filepath.Join(blocker, "call.log"), 400, 0))
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res := c.Result()
	if res.LogError == "" {
		t.Fatal("日志创建失败应写进 LogError")
	}
	if res.OutputPath != "" {
		t.Fatalf("没有日志文件时不该给出 OutputPath：%q", res.OutputPath)
	}
	if res.Output != "hello" {
		t.Fatalf("内存输出不受日志失败影响：%q", res.Output)
	}
}

// 日志写到一半失败（磁盘满、句柄被关）时，output_path 必须收回：
// 给模型一个读不全的文件，比直接说「没写成」更糟。
func TestCaptureDropsOutputPathWhenLogWriteFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "call.log")
	c := newCapture(SpecFor(path, 400, 0))
	defer c.Close()

	if err := c.log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write 不能被日志失败打断: %v", err)
	}
	res := c.Result()
	if res.LogError == "" {
		t.Fatal("写入失败应写进 LogError")
	}
	if res.OutputPath != "" {
		t.Fatalf("日志写失败后不该给出 OutputPath：%q", res.OutputPath)
	}
}
