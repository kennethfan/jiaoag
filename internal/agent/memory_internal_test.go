package agent

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/prompt"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func TestSystemIncludesMemoryLayer(t *testing.T) {
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
		Memory:       "构建用 make check",
	}

	a := &Agent{opts: base}
	got, err := a.system()
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	if !strings.Contains(got.Text, "跨会话项目记忆") || !strings.Contains(got.Text, "构建用 make check") {
		t.Fatal("记忆正文没有进入系统提示")
	}

	base.Memory = ""
	b := &Agent{opts: base}
	empty, err := b.system()
	if err != nil {
		t.Fatalf("system: %v", err)
	}
	if strings.Contains(empty.Text, "跨会话项目记忆") {
		t.Fatal("空记忆不应产生提示层")
	}
	if empty.Hash == got.Hash {
		t.Fatal("记忆变化必须改变系统提示哈希")
	}
}
