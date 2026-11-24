package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// modelFactory 记录 provider 闭包收到的会话副本：单次覆盖必须体现在这里，
// 而不是靠「任务里存了一份」自我证明。
type modelFactory struct {
	mu       sync.Mutex
	seen     []domain.Session
	provider llm.Provider
}

func (f *modelFactory) fn(sess domain.Session) (llm.Provider, string, error) {
	f.mu.Lock()
	f.seen = append(f.seen, sess)
	f.mu.Unlock()
	model := sess.Model
	if model == "" {
		model = "default-model"
	}
	return f.provider, model, nil
}

func (f *modelFactory) first(t *testing.T) domain.Session {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("供应商构造函数没有被调用")
	}
	return f.seen[0]
}

// replyProvider 立刻回一段文字，让任务能走到终态。
type replyProvider struct{}

func (replyProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (replyProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if err := fn(llm.Chunk{TextDelta: "好了"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

func taskModelApp(t *testing.T) (*app.App, *modelFactory) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	factory := &modelFactory{provider: replyProvider{}}
	err = a.ConfigureRuntime(app.RuntimeDeps{
		Provider: factory.fn,
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:     2,
		ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	return a, factory
}

func TestSubmitTaskWithModelOverride(t *testing.T) {
	a, factory := taskModelApp(t)
	ctx := context.Background()
	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "覆盖测试")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	task, err := a.SubmitTaskWith(ctx, sess.ID, "用便宜模型跑一次", app.SubmitOptions{
		Provider: "agnes",
		Model:    "agnes-3.0-flash",
	})
	if err != nil {
		t.Fatalf("SubmitTaskWith: %v", err)
	}
	if task.Provider != "agnes" || task.Model != "agnes-3.0-flash" {
		t.Fatalf("任务未记录覆盖：%+v", task)
	}
	waitTaskDone(t, a, sess.ID)

	seen := factory.first(t)
	if seen.Provider != "agnes" || seen.Model != "agnes-3.0-flash" {
		t.Fatalf("供应商收到 %s/%s，期望 agnes/agnes-3.0-flash", seen.Provider, seen.Model)
	}
	// 覆盖只作用于这一次任务：库里的会话仍是原值。
	stored, err := a.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored.Provider != "openai" || stored.Model != "gpt-x" {
		t.Fatalf("会话被改动了：%s/%s", stored.Provider, stored.Model)
	}
}

func TestSubmitTaskWithoutOverrideFollowsSession(t *testing.T) {
	a, factory := taskModelApp(t)
	ctx := context.Background()
	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "默认测试")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "普通任务")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if task.Provider != "" || task.Model != "" {
		t.Fatalf("未覆盖的任务不该记录模型：%+v", task)
	}
	waitTaskDone(t, a, sess.ID)
	if seen := factory.first(t); seen.Model != "gpt-x" {
		t.Fatalf("供应商收到模型 %q，期望会话默认 gpt-x", seen.Model)
	}
}
