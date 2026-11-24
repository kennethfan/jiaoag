package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/workspace"
)

func TestNewRootsValidatesAliases(t *testing.T) {
	primary := mustWorkspace(t, t.TempDir())
	lib := t.TempDir()

	if _, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "lib", Path: lib}}); err != nil {
		t.Fatalf("合法附加根被拒: %v", err)
	}
	if _, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "", Path: lib}}); err == nil {
		t.Fatal("空别名应当报错")
	}
	if _, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "Lib", Path: lib}}); err == nil {
		t.Fatal("大写别名应当报错")
	}
	if _, err := workspace.NewRoots(primary, []workspace.NamedRoot{
		{Alias: "lib", Path: lib},
		{Alias: "lib", Path: t.TempDir()},
	}); err == nil {
		t.Fatal("重复别名应当报错")
	}
	if _, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "lib", Path: primary.Root()}}); err == nil {
		t.Fatal("与主根同目录应当报错")
	}
	if _, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "lib", Path: filepath.Join(lib, "missing")}}); err == nil {
		t.Fatal("不存在的目录应当报错")
	}
}

func TestRootsResolveAliasAndPlainPath(t *testing.T) {
	primaryRoot, libRoot := t.TempDir(), t.TempDir()
	primary := mustWorkspace(t, primaryRoot)
	roots, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "lib", Path: libRoot}})
	if err != nil {
		t.Fatalf("NewRoots: %v", err)
	}

	ws, rel, alias, err := roots.Resolve("lib:internal/app.go")
	if err != nil {
		t.Fatalf("解析附加根: %v", err)
	}
	if ws.Root() != mustWorkspace(t, libRoot).Root() || rel != "internal/app.go" || alias != "lib" {
		t.Fatalf("解析结果不对：ws=%s rel=%s alias=%s", ws.Root(), rel, alias)
	}

	ws, rel, alias, err = roots.Resolve("internal/app.go")
	if err != nil {
		t.Fatalf("解析主根: %v", err)
	}
	if ws.Root() != primary.Root() || rel != "internal/app.go" || alias != "" {
		t.Fatalf("主根解析结果不对：ws=%s rel=%s alias=%s", ws.Root(), rel, alias)
	}

	if _, _, _, err := roots.Resolve("nope:a.txt"); !errors.Is(err, workspace.ErrUnknownRoot) {
		t.Fatalf("未知别名应返回 ErrUnknownRoot，得到 %v", err)
	}

	// 形如别名但没挂载的前缀一律报错：把 "a:b.txt" 悄悄当成主根文件名，
	// 会让拼错的别名变成「文件不存在」这种误导性错误。
	if _, _, _, err := roots.Resolve("a:b.txt"); !errors.Is(err, workspace.ErrUnknownRoot) {
		t.Fatalf("未知前缀应报 ErrUnknownRoot：%v", err)
	}
	// 不是合法别名形态的前缀仍按主根文件名处理（例如大写盘符或中文）。
	if _, rel, alias, err := roots.Resolve("Z:b.txt"); err != nil || alias != "" || rel != "Z:b.txt" {
		t.Fatalf("非别名形态被误判：rel=%s alias=%s err=%v", rel, alias, err)
	}
}

func TestRootsExtraAndJoin(t *testing.T) {
	primary := mustWorkspace(t, t.TempDir())
	libRoot := t.TempDir()
	roots, err := workspace.NewRoots(primary, []workspace.NamedRoot{{Alias: "lib", Path: libRoot}})
	if err != nil {
		t.Fatalf("NewRoots: %v", err)
	}
	extra := roots.Extra()
	if len(extra) != 1 || extra[0].Alias != "lib" {
		t.Fatalf("Extra 结果不对：%+v", extra)
	}
	if got := roots.Join(filepath.Join(extra[0].Path, "x.go")); got != "lib:x.go" {
		t.Fatalf("Join 附加根 = %q", got)
	}
	if got := roots.Join(filepath.Join(primary.Root(), "y.go")); got != "y.go" {
		t.Fatalf("Join 主根 = %q", got)
	}
	if got := roots.Join("/elsewhere/z.go"); got != "/elsewhere/z.go" {
		t.Fatalf("Join 根外 = %q", got)
	}
}

func mustWorkspace(t *testing.T, root string) *workspace.Workspace {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("创建目录: %v", err)
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}
