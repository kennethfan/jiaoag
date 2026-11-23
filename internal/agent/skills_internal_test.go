package agent

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/prompt"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func TestSystemIncludesSkillsIndex(t *testing.T) {
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	base := Options{
		Workspace:    ws,
		Tools:        registry,
		Model:        "test-model",
		SystemLayers: []prompt.Layer{prompt.Builtin()},
		SkillsIndex:  "## 可用技能（按需加载）\n\n- `pdf`：处理 PDF",
	}

	a := &Agent{opts: base}
	got, err := a.system()
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	if !strings.Contains(got.Text, "`pdf`：处理 PDF") {
		t.Fatal("技能索引没有进入系统提示")
	}
	// 索引排在核心之后：它不能覆盖安全边界，只能补充「有哪些技能」。
	if strings.Index(got.Text, "可用技能") < strings.Index(got.Text, "工作方式") {
		t.Fatal("技能索引应追加在内置核心之后")
	}

	base.SkillsIndex = ""
	b := &Agent{opts: base}
	empty, err := b.system()
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	if strings.Contains(empty.Text, "可用技能") {
		t.Fatal("空索引不应产生提示层")
	}
	if empty.Hash == got.Hash {
		t.Fatal("技能索引变化必须改变系统提示哈希")
	}
}
