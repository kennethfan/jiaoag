package project_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/project"
)

func prompt(path, text string) project.Prompt { return project.Prompt{Path: path, Text: text} }

func site(alias, root, digest string, mod func(*project.Project)) project.Site {
	p := &project.Project{Root: root, Digest: digest}
	if mod != nil {
		mod(p)
	}
	return project.Site{Alias: alias, Project: p}
}

// 主根在首位，附加根按别名升序；记忆要按这个顺序读，所以空根也必须保留。
func TestMergeOrdersSitesAndKeepsEmptyRoots(t *testing.T) {
	merged := project.Merge([]project.Site{
		site("zeta", "/z", "", nil),
		site("", "/ws", "", nil),
		site("alpha", "/a", "", nil),
	})
	if len(merged.Sites) != 3 {
		t.Fatalf("Sites = %d, want 3", len(merged.Sites))
	}
	got := []string{merged.Sites[0].Alias, merged.Sites[1].Alias, merged.Sites[2].Alias}
	want := []string{"", "alpha", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sites 顺序 = %v, want %v", got, want)
		}
	}
	if !merged.Empty() {
		t.Fatal("三个空根不应算有资产")
	}
}

// 单根工作区的摘要必须和 Discover 算出来的一字不差，否则升级后所有人的
// /trust 记录都会凭空失效。
func TestMergeSinglePrimaryKeepsDigest(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("规则\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := project.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest == "" {
		t.Fatal("Discover 没有算摘要")
	}
	if got := project.Merge([]project.Site{{Project: p}}).Digest; got != p.Digest {
		t.Fatalf("单根摘要 = %s, want %s", got, p.Digest)
	}
}

func TestMergeDigestTracksAliasPathAndContent(t *testing.T) {
	base := func() []project.Site {
		return []project.Site{
			site("", "/ws", "d0", func(p *project.Project) {
				p.Prompts = []project.Prompt{prompt("AGENTS.md", "主根")}
			}),
			site("lib", "/lib", "d1", func(p *project.Project) {
				p.Prompts = []project.Prompt{prompt("AGENTS.md", "附加根")}
			}),
		}
	}
	first := project.Merge(base()).Digest
	if first == "" {
		t.Fatal("有资产应产生摘要")
	}

	// 内容变化（Discover 会把内容算进每根的摘要，这里直接改它）
	changed := base()
	changed[1].Project.Digest = "d2"
	if got := project.Merge(changed).Digest; got == first {
		t.Fatal("内容变化后摘要应变化")
	}

	// 路径变化
	moved := base()
	moved[1].Project.Root = "/elsewhere"
	if got := project.Merge(moved).Digest; got == first {
		t.Fatal("路径变化后摘要应变化")
	}

	// 别名变化（同一目录换个别名，攻击面不同，摘要必须变）
	renamed := base()
	renamed[1].Alias = "vendor"
	if got := project.Merge(renamed).Digest; got == first {
		t.Fatal("别名变化后摘要应变化")
	}

	// 加一个没有资产的根不该让摘要失效
	withEmpty := append(base(), site("docs", "/docs", "", nil))
	if got := project.Merge(withEmpty).Digest; got != first {
		t.Fatalf("空根改变了摘要: %s != %s", got, first)
	}
}

func TestMergeSummarisesExtraRoots(t *testing.T) {
	merged := project.Merge([]project.Site{
		site("", "/ws", "d0", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", "a")}
			p.Servers = []project.Server{{Name: "repo", Argv: []string{"echo"}}}
		}),
		site("lib", "/lib", "d1", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", "b")}
			p.SkillDirs = []string{"/lib/.jiaoag/skills"}
		}),
	})
	got := merged.Summary()
	if !strings.Contains(got, "提示 2 个") || !strings.Contains(got, "技能 1 个") || !strings.Contains(got, "MCP 1 台") {
		t.Fatalf("Summary = %q", got)
	}
	if !strings.Contains(got, "主根 + 1 个附加根") {
		t.Fatalf("Summary 没有说明来源: %q", got)
	}
}

// 提示是累加的：主根在前（先占预算），附加根带别名前缀。
func TestMergePromptsPrimaryFirstWithAlias(t *testing.T) {
	merged := project.Merge([]project.Site{
		site("lib", "/lib", "d1", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", "lib 规则")}
		}),
		site("", "/ws", "d0", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", "ws 规则")}
		}),
	})
	prompts := merged.Prompts()
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d", len(prompts))
	}
	if prompts[0].Path != "AGENTS.md" || prompts[1].Path != "lib:AGENTS.md" {
		t.Fatalf("路径 = %q, %q", prompts[0].Path, prompts[1].Path)
	}
	if paths := merged.PromptPaths(); len(paths) != 2 || paths[1] != "lib:AGENTS.md" {
		t.Fatalf("PromptPaths = %v", paths)
	}
}

// 主根的提示先占满预算后，附加根拿不到额度（不能靠多挂仓库把上下文吃穿）。
func TestMergePromptsShareOneBudget(t *testing.T) {
	// 每段先被单文件上限截到 16 KB，两段正好吃满 32 KB 总预算。
	big := strings.Repeat("a", 16<<10)
	merged := project.Merge([]project.Site{
		site("", "/ws", "d0", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", big), prompt(".jiaoag/prompts/10.md", big)}
		}),
		site("lib", "/lib", "d1", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", "lib 规则")}
		}),
	})
	prompts := merged.Prompts()
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d, want 2（只剩主根的两段）", len(prompts))
	}
	for _, p := range prompts {
		if strings.Contains(p.Text, "lib 规则") {
			t.Fatal("附加根不该拿到额度")
		}
	}
	total := 0
	for _, p := range prompts {
		total += len(p.Text)
	}
	if total > 32<<10 {
		t.Fatalf("提示总量 %d 超过 32 KB 预算", total)
	}
}

// 技能是后出现者赢：附加根在前、主根在后。
func TestMergeSkillDirsPutPrimaryLast(t *testing.T) {
	merged := project.Merge([]project.Site{
		site("", "/ws", "d0", func(p *project.Project) {
			p.SkillDirs = []string{"/ws/.jiaoag/skills"}
		}),
		site("b", "/b", "d1", func(p *project.Project) {
			p.SkillDirs = []string{"/b/.jiaoag/skills"}
		}),
		site("a", "/a", "d2", func(p *project.Project) {
			p.SkillDirs = []string{"/a/.jiaoag/skills"}
		}),
	})
	got := merged.SkillDirs()
	want := []string{"/a/.jiaoag/skills", "/b/.jiaoag/skills", "/ws/.jiaoag/skills"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SkillDirs = %v, want %v", got, want)
	}
	names := merged.SkillNames()
	if strings.Join(names, ",") != "skills,a:skills,b:skills" {
		t.Fatalf("SkillNames = %v", names)
	}
}

// MCP server 同名时主根覆盖附加根，并记下是谁声明的。
func TestMergeServersPrimaryWinsAndKeepsOrigin(t *testing.T) {
	merged := project.Merge([]project.Site{
		site("lib", "/lib", "d1", func(p *project.Project) {
			p.Prompts = []project.Prompt{prompt("AGENTS.md", "x")}
			p.Servers = []project.Server{
				{Name: "repo", Argv: []string{"echo", "lib"}},
				{Name: "libfs", Argv: []string{"npx", "libfs"}},
			}
		}),
		site("", "/ws", "d0", func(p *project.Project) {
			p.Servers = []project.Server{{Name: "repo", Argv: []string{"echo", "ws"}}}
		}),
	})
	servers := merged.Servers()
	if len(servers) != 2 {
		t.Fatalf("servers = %d", len(servers))
	}
	byName := map[string]project.Server{}
	for _, s := range servers {
		byName[s.Name] = s
	}
	if got := byName["repo"]; got.Argv[1] != "ws" || got.Origin != "" {
		t.Fatalf("repo 应来自主根: %+v", got)
	}
	if got := byName["libfs"]; got.Origin != "lib" {
		t.Fatalf("libfs 的 Origin = %q", got.Origin)
	}
	names := merged.ServerNames()
	if names[0] != "repo: echo ws" || names[1] != "lib/libfs: npx libfs" {
		t.Fatalf("ServerNames = %v", names)
	}
}
