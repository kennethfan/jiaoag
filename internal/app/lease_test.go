package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
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

// holdProvider 卡在 release 上，用来把一个任务钉在 running。
type holdProvider struct{ release chan struct{} }

func (p *holdProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *holdProvider) ChatStream(ctx context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	if err := fn(llm.Chunk{TextDelta: "好了"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

// countProvider 记录被调用次数：它一次都不该被调用，说明任务根本没被执行。
type countProvider struct{ calls atomic.Int32 }

func (p *countProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *countProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	p.calls.Add(1)
	if err := fn(llm.Chunk{TextDelta: "不该跑到这里"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

func configureRuntimeID(t *testing.T, a *app.App, provider llm.Provider, runtimeID string, resume bool) {
	t.Helper()
	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:          3,
		ContextBudget:      10000,
		MaxConcurrentTasks: 4,
		ResumeQueued:       resume,
		RuntimeID:          runtimeID,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// 两个运行时共用一个库：同一会话里前一个任务还在跑时，后一个运行时
// 拿不到占位，绝不会把排队任务提前执行成第二个任务。
func TestSecondRuntimeCannotClaimBusySession(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	now := time.Now()

	db1, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if err := db1.RegisterRuntime(ctx, store.Runtime{
		ID: "rt_1", PID: 1, Host: "test", Kind: "tui", StartedAt: now, HeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterRuntime: %v", err)
	}
	a1 := app.New(db1, event.NewHub())
	defer a1.Close()

	db2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开第二个数据库句柄: %v", err)
	}
	if err := db2.RegisterRuntime(ctx, store.Runtime{
		ID: "rt_2", PID: 2, Host: "test", Kind: "tui", StartedAt: now, HeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterRuntime: %v", err)
	}
	a2 := app.New(db2, event.NewHub())
	defer a2.Close()

	hold := &holdProvider{release: make(chan struct{})}
	spare := &countProvider{}
	configureRuntimeID(t, a1, hold, "rt_1", false)
	configureRuntimeID(t, a2, spare, "rt_2", false)

	dir := t.TempDir()
	sess, err := a1.CreateSession(ctx, dir, "", "", "双运行时")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, err := a1.SubmitTask(ctx, sess.ID, "第一个任务")
	if err != nil {
		t.Fatalf("提交第一个任务: %v", err)
	}

	// 等第一个任务被 rt_1 占位并进入 running。
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := db1.GetTask(ctx, first.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Status == domain.TaskRunning {
			if task.Owner != "rt_1" {
				t.Fatalf("running 任务应当由 rt_1 占用，实际 %q", task.Owner)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("第一个任务没有进入 running：%+v", task)
		}
		time.Sleep(10 * time.Millisecond)
	}

	second, err := a2.SubmitTask(ctx, sess.ID, "第二个任务")
	if err != nil {
		t.Fatalf("从第二个运行时提交任务: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := spare.calls.Load(); n != 0 {
		t.Fatalf("同会话已有任务在跑，第二个运行时不应当执行任何任务，实际调用 %d 次", n)
	}
	task2, err := db1.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task2.Status != domain.TaskQueued {
		t.Fatalf("第二个任务应当还在排队，实际 %s", task2.Status)
	}

	close(hold.release)
	waitForStatus(t, db1, first.ID, domain.TaskCompleted)
}

func waitForStatus(t *testing.T, db *store.Store, id domain.TaskID, want domain.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := db.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务没有到达 %s：%+v", want, task)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
