package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func TestSnapshotReportsExistingFile(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, exists, err := ws.Snapshot("a.txt")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !exists || string(data) != "hello\n" {
		t.Fatalf("内容 = %q，exists = %v", data, exists)
	}

	hash, exists, err := ws.Hash("a.txt")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !exists || hash != patch.Hash([]byte("hello\n")) {
		t.Fatalf("摘要 = %q，exists = %v", hash, exists)
	}
}

func TestSnapshotMissingFileIsNotAnError(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	data, exists, err := ws.Snapshot("nope.txt")
	if err != nil {
		t.Fatalf("不存在的文件不该报错：%v", err)
	}
	if exists || data != nil {
		t.Fatalf("不存在时应返回 exists=false，实际 %v / %q", exists, data)
	}
	if _, exists, err := ws.Hash("nope.txt"); err != nil || exists {
		t.Fatalf("Hash 不存在文件：exists=%v err=%v", exists, err)
	}
}

func TestSnapshotRejectsBinary(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte{0x00, 0x01}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Snapshot("bin"); err == nil {
		t.Fatal("含 NUL 的文件应被拒绝")
	}
}

func TestWriteCreatesAndReplaces(t *testing.T) {
	ws, dir := newTestWorkspace(t)

	if err := ws.Write("new.txt", []byte("第一版\n")); err != nil {
		t.Fatalf("Write 新建: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "第一版\n" {
		t.Fatalf("内容 = %q", got)
	}

	if err := ws.Write("new.txt", []byte("第二版\n")); err != nil {
		t.Fatalf("Write 覆盖: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(got) != "第二版\n" {
		t.Fatalf("覆盖后内容 = %q", got)
	}

	hash, _, err := ws.Hash("new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if hash != patch.Hash([]byte("第二版\n")) {
		t.Fatalf("写入后摘要与内容不一致")
	}
}

func TestWriteKeepsMode(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write("secret.txt", []byte("y")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %v，期望 0600", info.Mode().Perm())
	}
}

func TestWriteRefusesOutsideAndBinary(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	if err := ws.Write("../escape.txt", []byte("x")); err == nil {
		t.Fatal("越界路径应被拒绝")
	}
	if err := ws.Write("bin", []byte{0x00}); err == nil {
		t.Fatal("二进制内容应被拒绝")
	}
}

func TestWriteRefusesMissingParent(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	if err := ws.Write("no/such/dir/a.txt", []byte("x")); err == nil {
		t.Fatal("父目录不存在时应失败，而不是隐式建目录")
	}
}

func TestWriteRefusesIgnoredPath(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	if err := ws.Write(".git/config", []byte("x")); err == nil {
		t.Fatal(".git 内部路径应被拒绝")
	}
}

// 目标是符号链接时读写都必须拒绝：rename 替换的是链接本身，而提案的 diff
// 是按链接指向的内容算的——放行一次，「批准的和落盘的」就成了两件事。
func TestSymlinkTargetIsNotRegular(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ws.Snapshot("link.txt"); !errors.Is(err, workspace.ErrNotRegular) {
		t.Fatalf("Snapshot 应拒绝符号链接，实际 %v", err)
	}
	if err := ws.Write("link.txt", []byte("bye\n")); !errors.Is(err, workspace.ErrNotRegular) {
		t.Fatalf("Write 应拒绝符号链接，实际 %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("链接指向的文件不该被改写，实际 %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "link.txt")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("链接本身应保持原样")
	}
}
