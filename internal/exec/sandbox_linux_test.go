//go:build linux

package exec_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
)

func TestSandboxBwrapWritesOnlyInWorkspace(t *testing.T) {
	b, err := execpkg.NewSandbox()
	if err != nil {
		t.Skipf("bwrap 不可用: %v", err)
	}
	ws := t.TempDir()
	outside, err := os.MkdirTemp(".", ".sandbox-out-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	abs, _ := filepath.Abs(outside)

	run := func(script string) execpkg.Result {
		plan, err := b.Prepare(context.Background(), execpkg.Command{
			Argv:    []string{"/bin/sh", "-c", script},
			Cwd:     ws,
			Timeout: 10 * time.Second,
			Env: []string{
				"HOME=" + t.TempDir(),
				"PATH=/usr/bin:/bin",
				"TMPDIR=" + ws,
			},
		})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		res, err := b.Run(context.Background(), plan, execpkg.DefaultSpec(""))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res
	}

	if res := run("echo ok > inside.txt"); res.ExitCode != 0 {
		t.Fatalf("工作区内写入失败：%q", res.Output)
	}
	if res := run("echo bad > " + abs + "/f"); res.ExitCode == 0 {
		t.Fatalf("工作区外写入不应成功：%q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(abs, "f")); err == nil {
		t.Fatal("沙箱外的文件被写出来了")
	}
}
