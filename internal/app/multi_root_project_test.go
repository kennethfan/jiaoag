package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// multiRootEnv 是「主根 + 一个附加根」的项目级扩展布局：
//
//	ws/AGENTS.md              主根规则 REPO-MAIN
//	ws/MEMORY.md              主根记忆 MEM-MAIN
//	ws/.jiaoag/skills/style   主根版技能
//	ws/.jiaoag/mcp.yaml       name: repo（主根声明）
//	lib/AGENTS.md             附加根规则 REPO-LIB
//	lib/MEMORY.md             附加根记忆 MEM-LIB
//	lib/.jiaoag/skills/style  附加根版技能（与主根同名）
//	lib/.jiaoag/mcp.yaml      name: repo（同名，应被主根覆盖）+ name: libfs
type multiRootEnv struct {
	app    *app.App
	sess   domain.Session
	roots  []domain.SessionRoot
	wsDir  string
	libDir string
	// extra 是最近一次 MCPServers 收到的项目级清单。
	extra []mcppkg.Server
}

func newMultiRootEnv(t *testing.T, provider llm.Provider, mode string) *multiRootEnv {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	db, err := store.Open(ctx, filepath.Join(root, "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	env := &multiRootEnv{
		app:    a,
		wsDir:  filepath.Join(root, "ws"),
		libDir: filepath.Join(root, "lib"),
	}
	writeFile(t, filepath.Join(env.wsDir, "AGENTS.md"), "主根规则：REPO-MAIN。\n")
	writeFile(t, filepath.Join(env.wsDir, "MEMORY.md"), "- 2026-01-01: 主根记忆 MEM-MAIN\n")
	writeFile(t, filepath.Join(env.wsDir, ".jiaoag", "skills", "style", "SKILL.md"),
		"---\nname: style\ndescription: 主根版\n---\n")
	writeFile(t, filepath.Join(env.wsDir, ".jiaoag", "mcp.yaml"),
		"servers:\n  - name: repo\n    command: echo\n    args: [\"ws\"]\n")

	writeFile(t, filepath.Join(env.libDir, "AGENTS.md"), "附加根规则：REPO-LIB。\n")
	writeFile(t, filepath.Join(env.libDir, "MEMORY.md"), "- 2026-01-01: 附加根记忆 MEM-LIB\n")
	writeFile(t, filepath.Join(env.libDir, ".jiaoag", "skills", "style", "SKILL.md"),
		"---\nname: style\ndescription: 附加根版\n---\n")
	writeFile(t, filepath.Join(env.libDir, ".jiaoag", "mcp.yaml"),
		"servers:\n"+
			"  - name: repo\n    command: echo\n    args: [\"lib\"]\n"+
			"  - name: libfs\n    command: npx\n    args: [\"libfs\"]\n")

	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MCPServers: func(_ string, extra []mcppkg.Server) []mcppkg.ServerStatus {
			env.extra = extra
			out := make([]mcppkg.ServerStatus, 0, len(extra))
			for _, srv := range extra {
				out = append(out, mcppkg.ServerStatus{
					Name:   srv.Name,
					Argv:   srv.Argv,
					Origin: srv.Origin,
					Source: sourceOf(srv),
				})
			}
			return out
		},
		// 记忆默认关；多根记忆的注入要靠它打开。
		Memory:        true,
		MaxRounds:     3,
		ContextBudget: 10000,
		ProjectMode:   mode,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	env.roots = []domain.SessionRoot{{Alias: "lib", Path: env.libDir}}
	sess, err := a.CreateSession(ctx, env.wsDir, "", "", "多根项目级", env.roots...)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	env.sess = sess
	return env
}

// sourceOf 复刻 mcp.Status 的来源标记，让 App 层测试能校验别名。
func sourceOf(srv mcppkg.Server) string {
	if srv.Origin == "" {
		return "project"
	}
	return "project:" + srv.Origin
}

func (e *multiRootEnv) runTask(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	if _, err := e.app.SubmitTask(ctx, e.sess.ID, "只回复 ok"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	task := waitTaskDone(t, e.app, e.sess.ID)
	if task.Status != domain.TaskCompleted {
		t.Fatalf("任务终态 = %s", task.Status)
	}
	return ""
}

// 未确认时附加根整体不参与：提示、技能、记忆都不进来。
func TestAdditionalRootProjectSkippedUntilTrusted(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	env := newMultiRootEnv(t, provider, "ask")

	status, err := env.app.ProjectStatus(ctx, env.wsDir, env.roots, "ask")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if !status.Detected || status.Trusted || status.Loaded {
		t.Fatalf("状态 = %+v", status)
	}
	if !strings.Contains(status.Summary, "主根 + 1 个附加根") {
		t.Fatalf("Summary = %q", status.Summary)
	}
	if !containsString(status.Prompts, "lib:AGENTS.md") {
		t.Fatalf("Prompts = %v", status.Prompts)
	}

	env.runTask(t)
	system := systemPromptOf(t, provider)
	// 未确认是整体跳过：主根规则也不进上下文（ADR-0025 原有语义）。
	if strings.Contains(system, "REPO-MAIN") || strings.Contains(system, "REPO-LIB") {
		t.Fatal("未确认时项目提示整体不该进上下文")
	}
	if !strings.Contains(system, "MEM-MAIN") {
		t.Fatal("主根记忆应生效")
	}
	if strings.Contains(system, "MEM-LIB") {
		t.Fatal("未确认时附加根记忆不该进上下文")
	}
	if strings.Contains(system, "附加根版") {
		t.Fatal("未确认时附加根技能不该覆盖主根")
	}
}

// 确认后三类资产一起进来，且主根优先。
func TestTrustedAdditionalRootInjectsPromptsSkillsAndMemory(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	env := newMultiRootEnv(t, provider, "ask")

	status, err := env.app.TrustProject(ctx, env.wsDir, env.roots, "ask")
	if err != nil {
		t.Fatalf("TrustProject: %v", err)
	}
	if !status.Loaded {
		t.Fatalf("确认后应加载: %+v", status)
	}

	env.runTask(t)
	system := systemPromptOf(t, provider)
	mainIdx := strings.Index(system, "REPO-MAIN")
	libIdx := strings.Index(system, "REPO-LIB")
	if mainIdx < 0 || libIdx < 0 {
		t.Fatalf("两条规则都应生效: main=%d lib=%d", mainIdx, libIdx)
	}
	if mainIdx > libIdx {
		t.Fatal("主根的提示应排在附加根前面")
	}
	if !strings.Contains(system, "### 主根") || !strings.Contains(system, "### lib") {
		t.Fatal("多根记忆应分段标注来源")
	}
	if !strings.Contains(system, "MEM-MAIN") || !strings.Contains(system, "MEM-LIB") {
		t.Fatal("两边的记忆都应注入")
	}
	if !strings.Contains(system, "主根版") {
		t.Fatal("主根技能应在索引里")
	}
	if strings.Contains(system, "附加根版") {
		t.Fatal("同名技能应由主根胜出")
	}
}

// 同名 MCP server 由主根覆盖，附加根独有的那台带别名来源。
func TestAdditionalRootMCPServersMergeWithPrimaryWinning(t *testing.T) {
	ctx := context.Background()
	env := newMultiRootEnv(t, &captureProvider{}, "ask")
	if _, err := env.app.TrustProject(ctx, env.wsDir, env.roots, "ask"); err != nil {
		t.Fatalf("TrustProject: %v", err)
	}

	statuses := env.app.MCPServers(env.wsDir, env.roots)
	byName := map[string]mcppkg.ServerStatus{}
	for _, st := range statuses {
		byName[st.Name] = st
	}
	repo, ok := byName["repo"]
	if !ok {
		t.Fatalf("缺少 repo: %+v", statuses)
	}
	if repo.Origin != "" || strings.Join(repo.Argv, " ") != "echo ws" {
		t.Fatalf("repo 应由主根覆盖: %+v", repo)
	}
	libfs, ok := byName["libfs"]
	if !ok {
		t.Fatalf("缺少 libfs: %+v", statuses)
	}
	if libfs.Origin != "lib" || libfs.Source != "project:lib" {
		t.Fatalf("libfs 来源 = %q / %q", libfs.Origin, libfs.Source)
	}

	// 改内容后摘要变化、需要重新确认——加挂的根不能靠一次确认永久生效。
	writeFile(t, filepath.Join(env.libDir, "AGENTS.md"), "附加根规则：REPO-LIB-CHANGED。\n")
	st, err := env.app.ProjectStatus(ctx, env.wsDir, env.roots, "ask")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if st.Trusted {
		t.Fatal("附加根内容变化后应要求重新确认")
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
