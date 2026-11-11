package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/workspace"
)

func newTestWorkspace(t *testing.T) (*workspace.Workspace, string) {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ws, dir
}

func TestResolveRejectsEscape(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	for _, rel := range []string{"..", "../secret", "a/../../b"} {
		if _, err := ws.Resolve(rel); !errors.Is(err, workspace.ErrOutsideRoot) {
			t.Fatalf("Resolve(%q) 应拒绝，得到 %v", rel, err)
		}
	}
	if _, err := ws.Resolve("/etc/passwd"); !errors.Is(err, workspace.ErrOutsideRoot) {
		t.Fatalf("绝对路径应拒绝，得到 %v", err)
	}
}

func TestResolveRejectsGitDir(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	if _, err := ws.Resolve(".git/config"); !errors.Is(err, workspace.ErrIgnored) {
		t.Fatalf(".git 内路径应拒绝，得到 %v", err)
	}
}

func TestResolveRejectsSymlinkOutside(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Skipf("当前环境不支持符号链接：%v", err)
	}

	if _, err := ws.Resolve("link"); !errors.Is(err, workspace.ErrOutsideRoot) {
		t.Fatalf("指向根外的链接应拒绝，得到 %v", err)
	}
}

func TestResolveAcceptsMissingFile(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	abs, err := ws.Resolve("nested/new/file.go")
	if err != nil {
		t.Fatalf("不存在的文件也应能解析：%v", err)
	}
	if !strings.HasPrefix(abs, ws.Root()) {
		t.Fatalf("解析结果 %q 不在根目录下", abs)
	}
}

func TestReadPaginatesAndReportsTruncation(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	content, err := ws.Read("many.txt", 3, 2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(content.Lines) != 2 || content.Lines[0] != "line" {
		t.Fatalf("分页结果不对：%v", content.Lines)
	}
	if content.TotalLines != 10 {
		t.Fatalf("总行数 = %d，期望 10", content.TotalLines)
	}
	if !content.Truncated {
		t.Fatal("只读了 2 行，应标记截断")
	}
}

func TestReadRejectsBinary(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Read("bin.dat", 0, 0); !errors.Is(err, workspace.ErrBinary) {
		t.Fatalf("二进制文件应拒绝，得到 %v", err)
	}
}

func TestReadRejectsDirectory(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Read("sub", 0, 0); !errors.Is(err, workspace.ErrNotRegular) {
		t.Fatalf("目录应拒绝，得到 %v", err)
	}
}

func TestWalkSkipsIgnoredDirs(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	files := []string{"a.go", "internal/b.go", "node_modules/x.js", ".git/HEAD", "vendor/v.go"}
	for _, rel := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	if err := ws.Walk(".", func(rel string, size int64) error {
		seen = append(seen, rel)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("应只看到 a.go 与 internal/b.go，实际 %v", seen)
	}
}

func TestReadDropsPartialTrailingLine(t *testing.T) {
	ws, dir := newTestWorkspace(t)
	// 2000 行 × 600 字节 = 1.2 MB，第 1 MB 落在某行中间，末行必被字节截断。
	line := strings.Repeat("x", 600)
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	content, err := ws.Read("big.txt", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !content.Truncated {
		t.Fatal("超过 1 MB 的文件应标记截断")
	}
	if len(content.Lines) > content.TotalLines {
		t.Fatalf("返回 %d 行超过 TotalLines %d：末尾残行未被丢弃", len(content.Lines), content.TotalLines)
	}
	for i, text := range content.Lines {
		if len(text) != 600 {
			t.Fatalf("第 %d 行长度 %d，期望 600（残行混入输出）", i+1, len(text))
		}
	}
}
