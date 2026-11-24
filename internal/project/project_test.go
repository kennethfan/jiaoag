package project_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/project"
)

// write 在 root 下写文件，自动建父目录。
func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverEmptyWorkspace(t *testing.T) {
	p, err := project.Discover(t.TempDir())
	if err != nil {
		t.Fatalf("空工作区不该报错：%v", err)
	}
	if !p.Empty() {
		t.Fatalf("应为空，得到 %+v", p)
	}
	if p.Digest != "" {
		t.Fatalf("空项目摘要应为空，得到 %q", p.Digest)
	}
	if p.Summary() != "" {
		t.Fatalf("空项目不该有摘要，得到 %q", p.Summary())
	}
}

func TestDiscoverCollectsPromptsSkillsAndServers(t *testing.T) {
	root := t.TempDir()
	write(t, root, "AGENTS.md", "仓库规则：改代码前先看测试。\n")
	write(t, root, ".jiaoag/prompts/20-later.md", "第二段提示\n")
	write(t, root, ".jiaoag/prompts/10-first.md", "第一段提示\n")
	write(t, root, ".jiaoag/prompts/README.txt", "非 md 忽略\n")
	write(t, root, ".jiaoag/skills/repo-style/SKILL.md", "---\nname: repo-style\ndescription: 仓库风格\n---\n")
	write(t, root, ".agents/skills/shared/SKILL.md", "---\nname: shared\ndescription: 共享技能\n---\n")
	write(t, root, ".jiaoag/mcp.yaml", `
servers:
  - name: repo
    command: npx
    args: ["-y", "some-server"]
    env:
      LEVEL: debug
      ALPHA: 1
    dir: sub
    timeout_s: 30
    trust: allow
    tools:
      include: [read_file]
      exclude: [write_file, edit_file]
  - name: off
    command: true
    enabled: false
`)

	p, err := project.Discover(root)
	if err != nil {
		t.Fatalf("发现失败：%v", err)
	}
	if want := []string{"AGENTS.md", ".jiaoag/prompts/10-first.md", ".jiaoag/prompts/20-later.md"}; !equal(p.PromptPaths(), want) {
		t.Fatalf("提示顺序不对：%v", p.PromptPaths())
	}
	if !strings.Contains(p.Prompts[0].Text, "仓库规则") || !strings.Contains(p.Prompts[1].Text, "第一段") {
		t.Fatalf("提示内容不对：%+v", p.Prompts)
	}
	if p.SkillDirs[0] != filepath.Join(root, ".jiaoag/skills") || p.SkillDirs[1] != filepath.Join(root, ".agents/skills") {
		t.Fatalf("技能目录不对：%v", p.SkillDirs)
	}
	if len(p.Servers) != 1 {
		t.Fatalf("禁用的 server 应被跳过，得到 %d 台", len(p.Servers))
	}
	srv := p.Servers[0]
	if srv.Name != "repo" || strings.Join(srv.Argv, " ") != "npx -y some-server" {
		t.Fatalf("server argv 不对：%+v", srv)
	}
	if strings.Join(srv.Env, ",") != "ALPHA=1,LEVEL=debug" {
		t.Fatalf("env 应按名字排序，得到 %v", srv.Env)
	}
	if srv.Dir != filepath.Join(root, "sub") {
		t.Fatalf("相对 dir 应按工作区解析，得到 %q", srv.Dir)
	}
	if !srv.Trust {
		t.Fatal("trust: allow 应映射为 true")
	}
	if !equal(srv.Include, []string{"read_file"}) || !equal(srv.Exclude, []string{"write_file", "edit_file"}) {
		t.Fatalf("工具过滤没解析出来：include=%v exclude=%v", srv.Include, srv.Exclude)
	}
	if p.Summary() != "提示 3 个、技能 2 个、MCP 1 台" {
		t.Fatalf("摘要不对：%q", p.Summary())
	}
	if len(p.Digest) != 64 {
		t.Fatalf("摘要应为 sha256 十六进制，得到 %q", p.Digest)
	}
}

func TestDigestChangesWithContentAndPaths(t *testing.T) {
	root := t.TempDir()
	write(t, root, "AGENTS.md", "版本一\n")
	base, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}

	write(t, root, "AGENTS.md", "版本二\n")
	changed, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == base.Digest {
		t.Fatal("内容变化必须改变摘要，否则信任会被静默复用")
	}

	// 改名不换内容也要变：改名会改变装配顺序。
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "AGENTS.md", "版本二\n")
	again, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != changed.Digest {
		t.Fatal("内容相同、路径相同应得到相同摘要")
	}

	write(t, root, ".jiaoag/prompts/10-a.md", "片段\n")
	withPrompt, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if withPrompt.Digest == again.Digest {
		t.Fatal("新增提示片段必须改变摘要")
	}
}

func TestDiscoverRejectsBadMCPDoc(t *testing.T) {
	cases := map[string]string{
		"名字非法":      "servers:\n  - name: \"bad name\"\n    command: x\n",
		"缺少命令":      "servers:\n  - name: ok\n",
		"超时越界":      "servers:\n  - name: ok\n    command: x\n    timeout_s: 601\n",
		"信任值非法":     "servers:\n  - name: ok\n    command: x\n    trust: maybe\n",
		"过滤项为空":     "servers:\n  - name: ok\n    command: x\n    tools:\n      include: [\"\"]\n",
		"名称重复":      "servers:\n  - name: ok\n    command: x\n  - name: ok\n    command: y\n",
		"yaml 解析失败": "servers: [\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ".jiaoag/mcp.yaml", doc)
			p, err := project.Discover(root)
			if err == nil {
				t.Fatalf("应报错，得到 %+v", p)
			}
			if len(p.Servers) != 0 {
				t.Fatalf("非法清单不该产出 server：%+v", p.Servers)
			}
			if !strings.Contains(err.Error(), "mcp.yaml") {
				t.Fatalf("错误应指出文件：%v", err)
			}
		})
	}
}

func TestDiscoverKeepsGoingOnPartialError(t *testing.T) {
	root := t.TempDir()
	write(t, root, "AGENTS.md", "提示\n")
	write(t, root, ".jiaoag/mcp.yaml", "servers: [\n")

	p, err := project.Discover(root)
	if err == nil {
		t.Fatal("坏清单应报错")
	}
	if len(p.Prompts) != 1 {
		t.Fatalf("提示仍应被发现，得到 %+v", p.Prompts)
	}
}

func TestPromptBudgetClipsLaterFiles(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("甲", 16<<10) // 恰好等于单文件上限
	write(t, root, "AGENTS.md", big)
	write(t, root, ".jiaoag/prompts/10-a.md", big)
	write(t, root, ".jiaoag/prompts/20-b.md", big)

	p, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Prompts) < 2 {
		t.Fatalf("预算内至少应留下 AGENTS.md 与第一段，得到 %d", len(p.Prompts))
	}
	if !strings.Contains(p.Prompts[1].Text, "已截断") {
		t.Fatal("后一段应被截断并标注，而不是静默丢弃")
	}
	total := 0
	for _, prompt := range p.Prompts {
		total += len(prompt.Text)
	}
	if total > 48<<10 {
		t.Fatalf("提示总量失控：%d 字节", total)
	}
}

func TestDiscoverIgnoresEmptyPromptFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "AGENTS.md", "   \n")
	write(t, root, ".jiaoag/prompts/10-a.md", "\n\n")

	p, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("空白文件不算项目资产：%+v", p)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
