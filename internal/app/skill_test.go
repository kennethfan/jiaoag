package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// captureProvider 记下每次请求的 System，用于断言技能正文真的进了上下文。
type captureProvider struct {
	mu       sync.Mutex
	systems  []string
	requests int
}

func (p *captureProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *captureProvider) ChatStream(_ context.Context, req llm.Request, fn func(llm.Chunk) error) error {
	p.mu.Lock()
	p.systems = append(p.systems, req.System)
	p.requests++
	p.mu.Unlock()
	if err := fn(llm.Chunk{TextDelta: "好了"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

// skillApp 建一个带技能目录的 App：技能库在临时目录里，工作区另给一个。
func skillApp(t *testing.T, provider llm.Provider) (*app.App, domain.SessionID) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	libDir := filepath.Join(root, "skills")
	skillDir := filepath.Join(libDir, "grilling")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "---\nname: grilling\ndescription: 反复追问\ndisable-model-invocation: true\n---\n\n逐个问题追问，直到方案站得住。\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	wsDir := filepath.Join(root, "ws")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		SkillDirs:     []string{libDir},
		MaxRounds:     3,
		ContextBudget: 10000,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	sess, err := a.CreateSession(ctx, wsDir, "", "", "技能测试")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return a, sess.ID
}

func waitTaskDone(t *testing.T, a *app.App, sessionID domain.SessionID) domain.Task {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := a.ListTasks(ctx, sessionID)
		if err == nil && len(tasks) > 0 {
			switch tasks[len(tasks)-1].Status {
			case domain.TaskCompleted, domain.TaskFailed, domain.TaskCanceled:
				return tasks[len(tasks)-1]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("任务没有在 5s 内结束")
	return domain.Task{}
}

// 用户点名的技能正文直接进系统提示：不靠模型自己调 skill 工具。
func TestSubmitTaskInjectsRequestedSkill(t *testing.T) {
	ctx := context.Background()
	provider := &captureProvider{}
	a, sessionID := skillApp(t, provider)

	task, err := a.SubmitTask(ctx, sessionID, "帮我 grill 这个方案", "grilling")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if len(task.Skills) != 1 || task.Skills[0] != "grilling" {
		t.Fatalf("任务应记住点名的技能，得到 %v", task.Skills)
	}
	done := waitTaskDone(t, a, sessionID)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("任务未完成：%s %s", done.Status, done.Error)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.systems) == 0 {
		t.Fatal("没有发起模型请求")
	}
	if !strings.Contains(provider.systems[0], "逐个问题追问") {
		t.Fatalf("技能正文没有进入系统提示：\n%s", provider.systems[0])
	}
	// 仅手动技能不进模型可自选的索引，但用户点名后必须能加载。
	if strings.Contains(provider.systems[0], "`grilling`：反复追问") {
		t.Fatal("disable-model-invocation 的技能不该出现在索引里")
	}
}

// 点名了不存在的技能要在提交时就失败，而不是安静地少加载一份正文。
func TestSubmitTaskRejectsUnknownSkill(t *testing.T) {
	ctx := context.Background()
	a, sessionID := skillApp(t, &captureProvider{})

	_, err := a.SubmitTask(ctx, sessionID, "随便", "no-such-skill")
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "no-such-skill") {
		t.Fatalf("错误应带上技能名：%v", err)
	}
	// 提交失败不该留下一个永远排不上的 queued 任务。
	tasks, err := a.ListTasks(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("失败提交不应落库，得到 %d 个任务", len(tasks))
	}
}
