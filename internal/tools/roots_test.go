package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestWriteToolsRejectAdditionalRoot(t *testing.T) {
	primary, roots := rootsFixture(t)

	write := tools.NewWriteFileWithRoots(primary, roots)
	if _, err := write.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "write_file", Args: []byte(`{"path":"lib:b.txt","content":"x"}`),
	}); err == nil {
		t.Fatal("write_file 写附加根应当报错")
	} else if !strings.Contains(err.Error(), "只读") {
		t.Fatalf("错误应说明只读：%v", err)
	}

	edit := tools.NewEditFileWithRoots(primary, roots)
	if _, err := edit.Prepare(context.Background(), tools.Call{
		ID: "c2", Name: "edit_file", Args: []byte(`{"path":"lib:b.txt","old_string":"beta","new_string":"x"}`),
	}); err == nil {
		t.Fatal("edit_file 改附加根应当报错")
	}

	// 主根照旧可写。
	if _, err := write.Prepare(context.Background(), tools.Call{
		ID: "c3", Name: "write_file", Args: []byte(`{"path":"c.txt","content":"gamma"}`),
	}); err != nil {
		t.Fatalf("主根写入不该受影响：%v", err)
	}
}
