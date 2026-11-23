package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/memory"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func rememberTool(t *testing.T) (*workspace.Workspace, tools.Tool) {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws, tools.NewRemember(ws)
}

func rememberCall(text string) tools.Call {
	return tools.Call{ID: "c1", Name: "remember", Args: jsonArgs(`{"text":` + strconv.Quote(text) + `}`)}
}

// 记忆追加只产出提案：此刻磁盘上不该出现文件。
func TestRememberAppendsEntryProposal(t *testing.T) {
	ws, tool := rememberTool(t)
	prepared, err := tool.Prepare(context.Background(), rememberCall("  构建用\tmake check "))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Change == nil {
		t.Fatal("remember 必须给出 Change")
	}
	if prepared.Change.Exists {
		t.Fatal("首次记忆不该标记为已存在")
	}
	if prepared.Change.BaseHash != "" {
		t.Fatalf("新文件不该有 base_hash：%q", prepared.Change.BaseHash)
	}
	content := string(prepared.Change.NewContent)
	want := regexp.MustCompile(`^- \d{4}-\d{2}-\d{2}: 构建用 make check\n$`)
	if !want.MatchString(content) {
		t.Fatalf("记忆格式不符：%q", content)
	}
	if !strings.Contains(prepared.Change.Diff, "+- ") {
		t.Fatalf("diff 缺少新增条目：\n%s", prepared.Change.Diff)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), memory.FileName)); !os.IsNotExist(err) {
		t.Fatalf("Prepare 不应落盘：%v", err)
	}
}

func TestRememberKeepsExistingEntries(t *testing.T) {
	ws, tool := rememberTool(t)
	old := "- 2026-01-01: 旧事实\n"
	if err := os.WriteFile(filepath.Join(ws.Root(), memory.FileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := tool.Prepare(context.Background(), rememberCall("新事实"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	content := string(prepared.Change.NewContent)
	if !strings.HasPrefix(content, old) {
		t.Fatalf("旧条目必须保留：%q", content)
	}
	if !strings.Contains(content, "新事实") {
		t.Fatalf("新条目缺失：%q", content)
	}
}

func TestRememberRejectsDuplicate(t *testing.T) {
	ws, tool := rememberTool(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), memory.FileName), []byte("- 2026-01-01: 已知事实\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Prepare(context.Background(), rememberCall("已知事实"))
	if err == nil || !strings.Contains(err.Error(), "已经") {
		t.Fatalf("重复内容应被拒绝，得到 %v", err)
	}
}

func TestRememberRejectsBadText(t *testing.T) {
	_, tool := rememberTool(t)
	for name, text := range map[string]string{
		"空":  "  ",
		"超长": strings.Repeat("字", memory.MaxEntryRunes+1),
	} {
		if _, err := tool.Prepare(context.Background(), rememberCall(text)); err == nil {
			t.Fatalf("%s 文本应被拒绝", name)
		}
	}
}

func TestRememberRejectsOversizedFile(t *testing.T) {
	ws, tool := rememberTool(t)
	filler := strings.Repeat("x", memory.MaxLoadBytes-20)
	if err := os.WriteFile(filepath.Join(ws.Root(), memory.FileName), []byte(filler+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Prepare(context.Background(), rememberCall("再来一条"))
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超过总量上限应被拒绝，得到 %v", err)
	}
}
