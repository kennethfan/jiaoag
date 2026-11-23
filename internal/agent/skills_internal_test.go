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

func TestSystemIncludesForcedSkillBodies(t *testing.T) {
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
		SkillBodies:  []SkillBody{{Name: "grilling", Text: "技能：grilling\n\n逐个问题追问"}},
	}
	a := &Agent{opts: base}
	got, err := a.system()
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	if !strings.Contains(got.Text, "逐个问题追问") {
		t.Fatal("点名技能的正文没有进入系统提示")
	}
	// 正文已在上下文里，必须明说，否则模型会再调一次 skill 工具白花一轮。
	if !strings.Contains(got.Text, "无需再调用 skill 工具") {
		t.Fatal("点名技能的提示层缺少「无需再调用 skill 工具」的说明")
	}
	// 正文同样只能补充，不能压过内置核心。
	if strings.Index(got.Text, "逐个问题追问") < strings.Index(got.Text, "工作方式") {
		t.Fatal("技能正文应追加在内置核心之后")
	}

	base.SkillBodies = nil
	b := &Agent{opts: base}
	plain, err := b.system()
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	if strings.Contains(plain.Text, "逐个问题追问") {
		t.Fatal("没有点名技能时不该出现正文")
	}
	if plain.Hash == got.Hash {
		t.Fatal("加载技能正文必须改变系统提示哈希")
	}
}
