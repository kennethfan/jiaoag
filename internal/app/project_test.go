package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// projectApp 建一个工作区里带项目级扩展、机器级技能目录同名冲突的 App。
//
//	machine/style  description: 机器版
//	ws/.jiaoag/skills/style  description: 项目版
func projectApp(t *testing.T, provider llm.Provider, mode string) (*app.App, domain.SessionID, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	db, err := store.Open(ctx, filepath.Join(root, "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	writeFile(t, filepath.Join(root, "machine", "style", "SKILL.md"),
		"---\nname: style\ndescription: 机器版\ndescription_x: 1\n---\n")
	writeFile(t, filepath.Join(root, "machine", "only-machine", "SKILL.md"),
		"---\nname: only-machine\ndescription: 只有机器版\n---\n")

	wsDir := filepath.Join(root, "ws")
	writeFile(t, filepath.Join(wsDir, "AGENTS.md"), "项目规则：提交前跑 make check。\n")
	writeFile(t, filepath.Join(wsDir, ".jiaoag", "prompts", "10-extra.md"), "额外提示：用中文回复。\n")
	writeFile(t, filepath.Join(wsDir, ".jiaoag", "skills", "style", "SKILL.md"),
		"---\nname: style\ndescription: 项目版\n---\n")
	writeFile(t, filepath.Join(wsDir, ".jiaoag", "mcp.yaml"),
		"servers:\n  - name: repo\n    command: echo\n    args: [\"hi\"]\n")

	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		SkillDirs:     []string{filepath.Join(root, "machine")},
		MaxRounds:     3,
		ContextBudget: 10000,
		ProjectMode:   mode,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	sess, err := a.CreateSession(ctx, wsDir, "", "", "项目级测试")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return a, sess.ID, wsDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func systemPromptOf(t *testing.T, provider *captureProvider) string {
	t.Helper()
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.systems) == 0 {
		t.Fatal("没有发起模型请求")
	}
	return provider.systems[0]
}

// 未确认时整体跳过：既不注入提示，也不让项目技能覆盖机器技能。
func TestUntrustedProjectIsSkipped(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	a, sessionID, wsDir := projectApp(t, provider, "ask")

	status, err := a.ProjectStatus(ctx, wsDir, nil, "ask")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if !status.Detected || status.Trusted || status.Loaded {
		t.Fatalf("应发现但未加载，得到 %+v", status)
	}
	if status.Summary != "提示 2 个、技能 1 个、MCP 1 台" {
		t.Fatalf("摘要不对：%q", status.Summary)
	}

	if _, err := a.SubmitTask(ctx, sessionID, "随便说一句"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if done := waitTaskDone(t, a, sessionID); done.Status != domain.TaskCompleted {
		t.Fatalf("任务未完成：%s %s", done.Status, done.Error)
	}
	system := systemPromptOf(t, provider)
	if strings.Contains(system, "提交前跑 make check") {
		t.Fatal("未确认的项目提示不该进系统提示")
	}
	if !strings.Contains(system, "`style`：机器版") {
		t.Fatal("未确认时机器级技能仍应生效")
	}
	if strings.Contains(system, "项目版") {
		t.Fatal("未确认的项目技能不该覆盖机器级")
	}
}

// 确认之后：提示进系统提示、项目技能覆盖机器级同名技能。
func TestTrustedProjectIsInjected(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	a, sessionID, wsDir := projectApp(t, provider, "ask")

	status, err := a.TrustProject(ctx, wsDir, nil, "ask")
	if err != nil {
		t.Fatalf("TrustProject: %v", err)
	}
	if !status.Trusted || !status.Loaded {
		t.Fatalf("确认后应加载，得到 %+v", status)
	}

	if _, err := a.SubmitTask(ctx, sessionID, "随便说一句"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if done := waitTaskDone(t, a, sessionID); done.Status != domain.TaskCompleted {
		t.Fatalf("任务未完成：%s %s", done.Status, done.Error)
	}
	system := systemPromptOf(t, provider)
	for _, want := range []string{"提交前跑 make check", "额外提示：用中文回复", "项目版", "only-machine"} {
		if !strings.Contains(system, want) {
			t.Fatalf("系统提示缺少 %q：\n%s", want, system)
		}
	}
	if strings.Contains(system, "`style`：机器版") {
		t.Fatal("项目级同名技能应覆盖机器级")
	}
}

// 内容变了就要重新确认：旧摘要不能继续放行新内容。
func TestChangedProjectNeedsReconfirmation(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	a, sessionID, wsDir := projectApp(t, provider, "ask")

	if _, err := a.TrustProject(ctx, wsDir, nil, "ask"); err != nil {
		t.Fatalf("TrustProject: %v", err)
	}
	writeFile(t, filepath.Join(wsDir, "AGENTS.md"), "项目规则：改成别的。\n")

	status, err := a.ProjectStatus(ctx, wsDir, nil, "ask")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if status.Loaded {
		t.Fatalf("内容变化后不该继续加载，得到 %+v", status)
	}

	if _, err := a.SubmitTask(ctx, sessionID, "随便说一句"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if done := waitTaskDone(t, a, sessionID); done.Status != domain.TaskCompleted {
		t.Fatalf("任务未完成：%s %s", done.Status, done.Error)
	}
	if strings.Contains(systemPromptOf(t, provider), "改成别的") {
		t.Fatal("摘要不匹配时不该注入新内容")
	}
}

// auto 模式不看信任记录：配置里显式关掉确认，就按「自己写的仓库」处理。
func TestAutoModeLoadsWithoutTrust(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	a, sessionID, wsDir := projectApp(t, provider, "auto")

	status, err := a.ProjectStatus(ctx, wsDir, nil, "auto")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if !status.Trusted || !status.Loaded {
		t.Fatalf("auto 应直接加载，得到 %+v", status)
	}

	if _, err := a.SubmitTask(ctx, sessionID, "随便说一句"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	waitTaskDone(t, a, sessionID)
	if !strings.Contains(systemPromptOf(t, provider), "提交前跑 make check") {
		t.Fatal("auto 模式应注入项目提示")
	}
}

// off 模式连发现都不做：报告里 Detected 应为假。
func TestOffModeIgnoresProject(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	a, sessionID, wsDir := projectApp(t, provider, "off")

	status, err := a.ProjectStatus(ctx, wsDir, nil, "off")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if status.Detected || status.Loaded || status.Mode != "off" {
		t.Fatalf("off 应完全忽略，得到 %+v", status)
	}

	if _, err := a.SubmitTask(ctx, sessionID, "随便说一句"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	waitTaskDone(t, a, sessionID)
	system := systemPromptOf(t, provider)
	if strings.Contains(system, "提交前跑 make check") || strings.Contains(system, "项目版") {
		t.Fatal("off 模式不该加载任何项目资产")
	}
}

// 撤销信任后立即回到未加载状态。
func TestRevokeProjectTrustStopsInjection(t *testing.T) {
	ctx := context.Background()
	a, _, wsDir := projectApp(t, &captureProvider{}, "ask")

	if _, err := a.TrustProject(ctx, wsDir, nil, "ask"); err != nil {
		t.Fatalf("TrustProject: %v", err)
	}
	if err := a.RevokeProjectTrust(ctx, wsDir); err != nil {
		t.Fatalf("RevokeProjectTrust: %v", err)
	}
	status, err := a.ProjectStatus(ctx, wsDir, nil, "ask")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if status.Trusted || status.Loaded {
		t.Fatalf("撤销后不该加载，得到 %+v", status)
	}
}

// 没有项目资产时确认要明确失败，而不是写一条空摘要进去。
func TestTrustProjectNeedsAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "jiaoag.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TrustProject(ctx, empty, nil, "ask"); err == nil {
		t.Fatal("空项目确认应报错")
	}
}
