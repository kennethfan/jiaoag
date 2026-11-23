//go:build darwin

package exec_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
)

func sandboxOrSkip(t *testing.T) *execpkg.Sandbox {
	t.Helper()
	b, err := execpkg.NewSandbox()
	if err != nil {
		t.Skipf("sandbox 不可用: %v", err)
	}
	return b
}

func sandboxCommand(t *testing.T, cwd, home string, script string) execpkg.Result {
	t.Helper()
	b := sandboxOrSkip(t)
	plan, err := b.Prepare(context.Background(), execpkg.Command{
		Argv:    []string{"/bin/sh", "-c", script},
		Cwd:     cwd,
		Timeout: 10 * time.Second,
		Env: []string{
			"HOME=" + home,
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
			"TMPDIR=" + t.TempDir(),
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

func TestSandboxAllowsWorkspaceWrite(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()
	res := sandboxCommand(t, ws, home, "echo ok > inside.txt && cat inside.txt")
	if res.ExitCode != 0 {
		t.Fatalf("工作区内写入应成功：exit=%d output=%q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Fatalf("输出 = %q", res.Output)
	}
	stored, err := os.ReadFile(filepath.Join(ws, "inside.txt"))
	if err != nil || strings.TrimSpace(string(stored)) != "ok" {
		t.Fatalf("磁盘内容 = %q, err = %v", stored, err)
	}
}

// 工作区之外（这里是仓库内的临时目录）必须拒绝写入，否则隔离只是文案。
func TestSandboxDeniesWriteOutsideWritableRoots(t *testing.T) {
	outside, err := os.MkdirTemp(".", ".sandbox-out-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	abs, err := filepath.Abs(outside)
	if err != nil {
		t.Fatal(err)
	}

	res := sandboxCommand(t, t.TempDir(), t.TempDir(), "echo bad > "+abs+"/f")
	if res.ExitCode == 0 {
		t.Fatalf("工作区外写入不应成功：%q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(abs, "f")); err == nil {
		t.Fatal("沙箱外的文件被写出来了")
	}
}

func TestSandboxDeniesSensitiveRead(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "secret-material-9f3a"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	res := sandboxCommand(t, t.TempDir(), home, "cat $HOME/.ssh/id")
	if res.ExitCode == 0 {
		t.Fatalf("读取 ~/.ssh 不应成功：%q", res.Output)
	}
	if strings.Contains(res.Output, secret) {
		t.Fatal("敏感文件内容泄漏进了输出")
	}
}
