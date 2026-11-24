package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// rootsFixture 建主根与附加根：主根有 a.txt，附加根有 b.txt。
func rootsFixture(t *testing.T) (*workspace.Workspace, *workspace.Roots) {
	t.Helper()
	primaryRoot, libRoot := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(primaryRoot, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("写 a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libRoot, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatalf("写 b.txt: %v", err)
	}
	primary, err := workspace.New(primaryRoot)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	roots, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "lib", Path: libRoot}})
	if err != nil {
		t.Fatalf("NewRoots: %v", err)
	}
	return primary, roots
}

func TestReadFileReadsAdditionalRoot(t *testing.T) {
	primary, roots := rootsFixture(t)
	tool := tools.NewReadFileWithRoots(primary, roots)

	p, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: "read_file", Args: []byte(`{"path":"lib:b.txt"}`)})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(p.Paths) != 1 || !strings.HasSuffix(p.Paths[0], "b.txt") {
		t.Fatalf("路径不对：%v", p.Paths)
	}
	res, err := tool.Execute(context.Background(), p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "beta") {
		t.Fatalf("内容不含附加根文件：%q", res.Content)
	}

	if _, err := tool.Prepare(context.Background(), tools.Call{ID: "c2", Name: "read_file", Args: []byte(`{"path":"nope:b.txt"}`)}); err == nil {
		t.Fatal("未知别名应当报错")
	}
}

func TestGrepAndGlobReachAdditionalRoot(t *testing.T) {
	primary, roots := rootsFixture(t)

	grep := tools.NewGrepWithRoots(primary, roots)
	p, err := grep.Prepare(context.Background(), tools.Call{ID: "c1", Name: "grep", Args: []byte(`{"pattern":"beta","path":"lib:."}`)})
	if err != nil {
		t.Fatalf("grep Prepare: %v", err)
	}
	res, err := grep.Execute(context.Background(), p)
	if err != nil {
		t.Fatalf("grep Execute: %v", err)
	}
	if !strings.Contains(res.Content, "lib:b.txt:1:beta") {
		t.Fatalf("grep 结果应带别名前缀：%q", res.Content)
	}

	glob := tools.NewGlobWithRoots(primary, roots)
	p, err = glob.Prepare(context.Background(), tools.Call{ID: "c2", Name: "glob", Args: []byte(`{"pattern":"lib:**/*.txt"}`)})
	if err != nil {
		t.Fatalf("glob Prepare: %v", err)
	}
	res, err = glob.Execute(context.Background(), p)
	if err != nil {
		t.Fatalf("glob Execute: %v", err)
	}
	if !strings.Contains(res.Content, "lib:b.txt") {
		t.Fatalf("glob 结果应带别名前缀：%q", res.Content)
	}
}

func TestWriteToolsTargetAdditionalRoot(t *testing.T) {
	primary, roots := rootsFixture(t)

	write := tools.NewWriteFileWithRoots(primary, roots)
	p, err := write.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "write_file", Args: []byte(`{"path":"lib:b.txt","content":"beta-2\n"}`),
	})
	if err != nil {
		t.Fatalf("write_file 写附加根应当通过：%v", err)
	}
	if p.Change == nil {
		t.Fatal("应当产出写入提案")
	}
	if p.Change.Root != "lib" || p.Change.Rel != "b.txt" || p.Change.Path != "lib:b.txt" {
		t.Fatalf("附加根提案的路径不对：%+v", *p.Change)
	}
	if len(p.Paths) != 1 || !strings.HasSuffix(p.Paths[0], "b.txt") {
		t.Fatalf("绝对路径应指向附加根：%v", p.Paths)
	}
	// 提案阶段不落盘。
	if data, err := os.ReadFile(filepath.Join(roots.Primary().Root(), "b.txt")); err == nil {
		t.Fatalf("附加根的文件不该出现在主根：%q", data)
	}

	edit := tools.NewEditFileWithRoots(primary, roots)
	p, err = edit.Prepare(context.Background(), tools.Call{
		ID: "c2", Name: "edit_file", Args: []byte(`{"path":"lib:b.txt","old_string":"beta","new_string":"BETA"}`),
	})
	if err != nil {
		t.Fatalf("edit_file 改附加根应当通过：%v", err)
	}
	if p.Change.Root != "lib" {
		t.Fatalf("edit_file 提案缺少根别名：%+v", *p.Change)
	}

	// 主根照旧可写，且不带别名。
	p, err = write.Prepare(context.Background(), tools.Call{
		ID: "c3", Name: "write_file", Args: []byte(`{"path":"c.txt","content":"gamma"}`),
	})
	if err != nil {
		t.Fatalf("主根写入不该受影响：%v", err)
	}
	if p.Change.Root != "" || p.Change.Path != "c.txt" {
		t.Fatalf("主根提案不应带别名：%+v", *p.Change)
	}
}

func TestRunCommandUsesAdditionalRoot(t *testing.T) {
	primary, roots := rootsFixture(t)
	env := tools.RuntimeEnv{Roots: roots, Backend: execpkg.NewDirect(), Timeout: 30 * time.Second}
	cmd := tools.NewCheckCommand(primary, env)

	p, err := cmd.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "check_command", Args: []byte(`{"argv":["pwd"],"root":"lib"}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if p.Command.Root != "lib" {
		t.Fatalf("命令应记录根别名：%+v", *p.Command)
	}
	if p.Command.Cwd == primary.Root() {
		t.Fatalf("cwd 应切到附加根：%s", p.Command.Cwd)
	}
	if len(p.Command.Writable) != 2 {
		t.Fatalf("可写根应包含主根与附加根：%v", p.Command.Writable)
	}

	if _, err := cmd.Prepare(context.Background(), tools.Call{
		ID: "c2", Name: "check_command", Args: []byte(`{"argv":["pwd"],"root":"nope"}`),
	}); err == nil {
		t.Fatal("未知根别名应当报错")
	}
}
