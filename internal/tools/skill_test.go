package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/skills"
	"github.com/kennethfan/jiaoag/internal/tools"
)

func skillCatalog(t *testing.T) *skills.Catalog {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "pdf")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: pdf\ndescription: 处理 PDF 时使用\n---\n\n# 步骤\n\n1. 运行 scripts/extract.py\n"
	if err := os.WriteFile(filepath.Join(dir, skills.FileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, errs := skills.Load([]string{root}, root)
	if len(errs) != 0 {
		t.Fatalf("加载技能失败：%v", errs)
	}
	return cat
}

func TestSkillSpecIsReadOnly(t *testing.T) {
	tool := tools.NewSkill(skillCatalog(t))
	spec := tool.Spec()
	if spec.Name != "skill" {
		t.Fatalf("工具名 = %q", spec.Name)
	}
	if len(spec.Effects) != 1 || spec.Effects[0] != tools.EffectRead {
		t.Fatalf("技能加载是纯读取，得到 %v", spec.Effects)
	}
	var schema map[string]any
	if err := json.Unmarshal(spec.Schema, &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON：%v", err)
	}
}

func TestSkillPrepareRejectsUnknownName(t *testing.T) {
	tool := tools.NewSkill(skillCatalog(t))
	_, err := tool.Prepare(context.Background(), tools.Call{Name: "skill", Args: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "name 不能为空") {
		t.Fatalf("缺 name 应报错，得到 %v", err)
	}

	_, err = tool.Prepare(context.Background(), tools.Call{Name: "skill", Args: json.RawMessage(`{"name":"nope"}`)})
	if err == nil || !strings.Contains(err.Error(), "pdf") {
		t.Fatalf("未知技能应把可用名字回给模型，得到 %v", err)
	}
}

func TestSkillExecuteReturnsBody(t *testing.T) {
	cat := skillCatalog(t)
	tool := tools.NewSkill(cat)
	prep, err := tool.Prepare(context.Background(), tools.Call{Name: "skill", Args: json.RawMessage(`{"name":"pdf"}`)})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := tool.Execute(context.Background(), prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("不该失败：%s", res.Content)
	}
	for _, want := range []string{"技能：pdf", "scripts/", "# 步骤", "基准目录"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("正文缺少 %q：\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "description: 处理 PDF") {
		t.Fatal("frontmatter 应被剥掉")
	}
}
