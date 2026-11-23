package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, dir, front, body string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	content := front + body
	if err := os.WriteFile(filepath.Join(path, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIndexesSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", "---\nname: alpha\ndescription: 处理 alpha 场景\n---\n\n# alpha 正文\n", "")
	writeSkill(t, root, "beta", "---\nname: beta\ndescription: 处理 beta\n---\n\n# beta\n", "")
	// 没有 frontmatter：用目录名，不报错。
	writeSkill(t, root, "gamma", "", "没有 frontmatter 的文件\n")
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	cat, errs := Load([]string{root}, root)
	if len(errs) != 0 {
		t.Fatalf("不该有错误：%v", errs)
	}
	if cat.Len() != 3 {
		t.Fatalf("技能数 = %d，期望 3（%v）", cat.Len(), cat.Names())
	}
	alpha, ok := cat.Get("alpha")
	if !ok || alpha.Description != "处理 alpha 场景" {
		t.Fatalf("alpha = %+v", alpha)
	}
	if _, ok := cat.Get("gamma"); !ok {
		t.Fatalf("无 frontmatter 的目录应按目录名索引：%v", cat.Names())
	}

	section := cat.Section()
	for _, want := range []string{"可用技能", "`alpha`", "处理 alpha 场景", "`skill`"} {
		if !strings.Contains(section, want) {
			t.Fatalf("索引缺少 %q：\n%s", want, section)
		}
	}
	if strings.Contains(section, "# alpha 正文") {
		t.Fatal("正文不该进索引")
	}
}

func TestLoadToleratesHandWrittenFrontmatter(t *testing.T) {
	root := t.TempDir()
	// 名字里带非法字符：退回目录名，不报错也不进索引成乱码。
	writeSkill(t, root, "broken", "---\nname: [unclosed\n---\n", "")
	// 真实技能库里常见：description 里有未加引号的 "Triggers: a, b"，
	// 整份 YAML 解析会失败，但描述本身是可用的。
	writeSkill(t, root, "triggered", "---\nname: triggered\ndescription: 使用场景说明。Triggers: 甲, 乙\n---\n", "")
	// 块标量写法。
	writeSkill(t, root, "block", "---\nname: block\ndescription: >\n  第一行\n  第二行\n---\n", "")

	cat, errs := Load([]string{root}, root)
	if len(errs) != 0 {
		t.Fatalf("手写 frontmatter 不该报错：%v", errs)
	}
	if _, ok := cat.Get("broken"); !ok {
		t.Fatalf("非法 name 应退回目录名：%v", cat.Names())
	}
	if s, _ := cat.Get("triggered"); !strings.Contains(s.Description, "Triggers: 甲, 乙") {
		t.Fatalf("带冒号的描述应完整保留，得到 %q", s.Description)
	}
	if s, _ := cat.Get("block"); s.Description != "第一行 第二行" {
		t.Fatalf("块标量应合并成一行，得到 %q", s.Description)
	}
}

func TestLoadMissingDirIsSilent(t *testing.T) {
	cat, errs := Load([]string{filepath.Join(t.TempDir(), "nope")}, "")
	if len(errs) != 0 {
		t.Fatalf("目录不存在不该报错：%v", errs)
	}
	if cat.Len() != 0 {
		t.Fatalf("应为空，得到 %v", cat.Names())
	}
	if cat.Section() != "" {
		t.Fatal("空目录不该产生提示段")
	}
}

func TestLoadRelativeDirUsesWorkspaceAndLaterWins(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, ".jiaoag/skills/local", "---\nname: alpha\ndescription: 项目版\n---\n", "")
	other := t.TempDir()
	writeSkill(t, other, "alpha", "---\nname: alpha\ndescription: 机器版\n---\n", "")

	cat, errs := Load([]string{other, ".jiaoag/skills"}, root)
	if len(errs) != 0 {
		t.Fatalf("不该有错误：%v", errs)
	}
	if cat.Len() != 1 {
		t.Fatalf("同名应去重，得到 %v", cat.Names())
	}
	if got, _ := cat.Get("alpha"); got.Description != "项目版" {
		t.Fatalf("后出现的目录（项目级）应覆盖机器级，得到 %q", got.Description)
	}
}

func TestBodyHasBaseDirAndStripsFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", "---\nname: alpha\ndescription: d\n---\n\n# 步骤\n\n1. 运行 scripts/run.py\n", "")
	if err := os.MkdirAll(filepath.Join(root, "alpha", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}

	cat, _ := Load([]string{root}, root)
	body, err := cat.Body("alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"技能：alpha", filepath.Join(root, "alpha"), "scripts/", "# 步骤"} {
		if !strings.Contains(body, want) {
			t.Fatalf("正文缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "description: d") {
		t.Fatal("frontmatter 应该被剥掉")
	}

	if _, err := cat.Body("nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("未知技能应报错，得到 %v", err)
	}
}

func TestManualSkillsStayOutOfModelIndex(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "auto-one", "---\nname: auto-one\ndescription: 模型可以自己挑\n---\n\n正文 A\n", "")
	writeSkill(t, root, "manual-one", "---\nname: manual-one\ndescription: 只能用户点名\ndisable-model-invocation: true\n---\n\n正文 B\n", "")

	cat, errs := Load([]string{root}, root)
	if len(errs) != 0 {
		t.Fatalf("不该有错误：%v", errs)
	}
	if cat.Len() != 2 {
		t.Fatalf("两个技能都该被加载，得到 %v", cat.Names())
	}
	if got, _ := cat.Get("manual-one"); !got.Manual {
		t.Fatal("disable-model-invocation: true 应标记为仅手动")
	}
	if got, _ := cat.Get("auto-one"); got.Manual {
		t.Fatal("没有该字段的技能不该是仅手动")
	}

	section := cat.Section()
	if !strings.Contains(section, "`auto-one`") {
		t.Fatalf("自动技能应进索引：\n%s", section)
	}
	if strings.Contains(section, "`manual-one`") {
		t.Fatalf("仅手动技能不该进索引：\n%s", section)
	}
	// 列表接口仍要能看到它，否则用户没法显式指定。
	found := false
	for _, s := range cat.Skills() {
		if s.Name == "manual-one" && s.Manual {
			found = true
		}
	}
	if !found {
		t.Fatalf("Skills() 应包含仅手动技能：%v", cat.Names())
	}
	// 显式加载正文不受该标记影响。
	body, err := cat.Body("manual-one")
	if err != nil || !strings.Contains(body, "正文 B") {
		t.Fatalf("显式加载应成功：%v %q", err, body)
	}
}

func TestResolveDirExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := resolveDir("~/skills", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, "skills") {
		t.Fatalf("resolveDir = %q", got)
	}
	if _, err := resolveDir("relative", ""); err == nil {
		t.Fatal("没有工作区根时相对目录应报错")
	}
}
