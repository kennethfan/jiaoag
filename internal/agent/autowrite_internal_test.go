package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSensitiveWriteTable(t *testing.T) {
	sensitive := []string{
		".env",
		".env.local",
		"config/.env",
		".git/hooks/pre-commit",
		".jiaoag/mcp.yaml",
		"AGENTS.md",
		"MEMORY.md",
		"id_rsa",
		"deploy/keys.pem",
		".github/workflows/ci.yml",
		".npmrc",
	}
	for _, rel := range sensitive {
		if !sensitiveWrite(rel) {
			t.Errorf("%s 应保留审批", rel)
		}
	}
	normal := []string{"notes/a.md", "src/main.go", "README.md", ".golangci.yml", "docs/AGENTS-guide.md"}
	for _, rel := range normal {
		if sensitiveWrite(rel) {
			t.Errorf("%s 不该被判为敏感", rel)
		}
	}
}

func TestMatchRootPicksDeepestAndNamesTemp(t *testing.T) {
	roots := []string{"/ws", "/private/tmp"}
	root, kind, ok := matchRoot("/private/tmp/proj/a.txt", roots)
	if !ok || root != "/private/tmp" || kind != "tmp" {
		t.Fatalf("tmp 判定不对: root=%q kind=%q ok=%v", root, kind, ok)
	}
	root, kind, ok = matchRoot("/ws/sub/a.txt", roots)
	if !ok || root != "/ws" || kind != "workspace" {
		t.Fatalf("workspace 判定不对: root=%q kind=%q ok=%v", root, kind, ok)
	}
	// 工作区落在临时目录下时，取更深的那个作为依据。
	root, kind, ok = matchRoot("/private/tmp/proj/a.txt", []string{"/private/tmp", "/private/tmp/proj"})
	if !ok || root != "/private/tmp/proj" || kind != "workspace" {
		t.Fatalf("应取最深的根: root=%q kind=%q ok=%v", root, kind, ok)
	}
	if _, _, ok := matchRoot("/etc/passwd", roots); ok {
		t.Fatal("/etc 不在允许的根内")
	}
	if _, _, ok := matchRoot("/wsx/a.txt", roots); ok {
		t.Fatal("/wsx 不是 /ws 的子路径，前缀比较必须按目录边界")
	}
}

func TestResolveWritePathFollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	inside := filepath.Join(base, "inside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(inside, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	got := resolveWritePath(filepath.Join(link, "new.txt"))
	realOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realOutside, "new.txt")
	if got != want {
		t.Fatalf("应解析到真实路径 %q，实际 %q", want, got)
	}
}
