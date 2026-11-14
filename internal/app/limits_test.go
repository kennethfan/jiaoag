package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// configureLimitedRuntime 与 queue_test.go 的 runtime 接线一致，只多了并发/队列上限。
func configureLimitedRuntime(t *testing.T, a *app.App, p llm.Provider, concurrent, depth int) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return p, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:          3,
		ContextBudget:      10000,
		MaxConcurrentTasks: concurrent,
		MaxQueueDepth:      depth,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// 排队满时必须在落库之前拒绝：被拒的任务不该留下一条永远排不上的 queued。
func TestQueueDepthRejectsSubmitBeforePersist(t *testing.T) {
	a := newApp(t)
	prov := &stepProvider{release: make(chan struct{}, 4)}
	configureLimitedRuntime(t, a, prov, 0, 1)

	ctx := context.Background()
	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	first, err := a.SubmitTask(ctx, sess.ID, "第一个")
	if err != nil {
		t.Fatalf("第一个: %v", err)
	}
	waitStatus(t, a, first.ID, domain.TaskRunning)

	second, err := a.SubmitTask(ctx, sess.ID, "第二个")
	if err != nil {
		t.Fatalf("第二个: %v", err)
	}
	if second.Status != domain.TaskQueued {
		t.Fatalf("第二个状态 = %s，期望 queued", second.Status)
	}

	// 队列上限是 1，第三个必须被拒。
	if _, err := a.SubmitTask(ctx, sess.ID, "第三个"); !errors.Is(err, app.ErrQueueFull) {
		t.Fatalf("第三个应返回 ErrQueueFull，得到 %v", err)
	}
	tasks, err := a.ListTasks(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("被拒的任务不该落库：任务数 = %d（%v）", len(tasks), tasks)
	}

	// 收尾，避免留下运行中的协程。
	prov.release <- struct{}{}
	waitStatus(t, a, first.ID, domain.TaskCompleted)
	prov.release <- struct{}{}
	waitStatus(t, a, second.ID, domain.TaskCompleted)
}

// 并发槽满时，另一个会话的任务不能偷偷跑起来；槽一释放就该被 pump 拉起。
func TestConcurrentCapQueuesOtherSessionUntilSlotFrees(t *testing.T) {
	a := newApp(t)
	prov := &stepProvider{release: make(chan struct{}, 4)}
	configureLimitedRuntime(t, a, prov, 1, 16)

	ctx := context.Background()
	dir := t.TempDir()
	sessA, err := a.CreateSession(ctx, dir, "openai", "test-model", "A")
	if err != nil {
		t.Fatalf("CreateSession A: %v", err)
	}
	sessB, err := a.CreateSession(ctx, dir, "openai", "test-model", "B")
	if err != nil {
		t.Fatalf("CreateSession B: %v", err)
	}

	first, err := a.SubmitTask(ctx, sessA.ID, "A 的任务")
	if err != nil {
		t.Fatalf("提交 A: %v", err)
	}
	waitStatus(t, a, first.ID, domain.TaskRunning)

	other, err := a.SubmitTask(ctx, sessB.ID, "B 的任务")
	if err != nil {
		t.Fatalf("提交 B: %v", err)
	}
	// 槽只有一个：B 必须停在 queued，不能与 A 并行。
	time.Sleep(100 * time.Millisecond)
	task, err := a.GetTask(ctx, other.ID)
	if err != nil {
		t.Fatalf("GetTask B: %v", err)
	}
	if task.Status != domain.TaskQueued {
		t.Fatalf("并发槽满时 B 状态 = %s，期望 queued", task.Status)
	}

	// 取消 A 让出槽位：B 应当被 pump 拉起，而不是等到下一次提交。
	if err := a.CancelTask(ctx, sessA.ID, first.ID); err != nil {
		t.Fatalf("取消 A: %v", err)
	}
	waitStatus(t, a, first.ID, domain.TaskCanceled)
	waitStatus(t, a, other.ID, domain.TaskRunning)

	prov.release <- struct{}{}
	waitStatus(t, a, other.ID, domain.TaskCompleted)
}
