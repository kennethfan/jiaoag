package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// fakeProvider 先回一段文本；block 为 true 时一直等到 ctx 取消，
// 用来观察运行中任务的行为。
type fakeProvider struct {
	block bool
}

func (p *fakeProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *fakeProvider) ChatStream(ctx context.Context, req llm.Request, fn func(llm.Chunk) error) error {
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := fn(llm.Chunk{TextDelta: "好的"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 7, CompletionTokens: 3}})
}

func configureRuntime(t *testing.T, a *app.App, provider llm.Provider) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws))
		},
		MaxRounds:     3,
		ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// waitTerminal 等待任务进入终态。默认窗口按纯内存测试定：它们不该慢。
func waitTerminal(t *testing.T, a *app.App, id domain.TaskID) domain.Task {
	t.Helper()
	return waitTerminalIn(t, a, id, 5*time.Second)
}

// waitTerminalIn 允许真实执行子进程的用例放宽窗口。
//
// `go test ./...` 要编译并运行一个子进程，全量并行跑套件时会因为构建缓存与 CPU
// 争用从 1 秒涨到数秒：5 秒窗口失败的是等待窗口，不是产品行为（单独跑该用例
// 一直很快）。窗口应该长于执行的超时预算，否则命令超时会被误报成「任务没结束」。
func waitTerminalIn(t *testing.T, a *app.App, id domain.TaskID, timeout time.Duration) domain.Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := a.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("读取任务: %v", err)
		}
		if task.Status.Terminal() {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := a.GetTask(context.Background(), id)
	t.Fatalf("任务未在 %s 内结束，当前状态 %s", timeout, task.Status)
	return domain.Task{}
}

func waitRunning(t *testing.T, a *app.App, id domain.TaskID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := a.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("读取任务: %v", err)
		}
		if task.Status == domain.TaskRunning {
			return
		}
		// 瞬时完成的任务（例如 provider 立刻 panic）会跳过 running 窗口；
		// 在这里直接失败，别让调用方等到 5 秒超时才发现等错了东西。
		if task.Status.Terminal() {
			t.Fatalf("任务 %s 已直接到达 %s：waitRunning 只适用于 running 会持续的任务", id, task.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("任务没有进入 running")
}

func TestSubmitTaskRunsToCompletion(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	configureRuntime(t, a, &fakeProvider{})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess, err := a.CreateSession(ctx, dir, "openai", "test-model", "运行测试")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	task, err := a.SubmitTask(ctx, sess.ID, "你好")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	if task.SystemPromptHash == "" || task.ExtensionsDigest == "" {
		t.Fatalf("任务应冻结提示词与扩展指纹：%+v", task)
	}

	done := waitTerminal(t, a, task.ID)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s，期望 completed（error=%q）", done.Status, done.Error)
	}
	if done.PromptTokens != 7 || done.CompleteTokens != 3 {
		t.Fatalf("token 未落库：%+v", done)
	}
}

func TestSubmitTaskQueuesWhenSessionBusy(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	configureRuntime(t, a, &fakeProvider{block: true})

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.SubmitTask(ctx, sess.ID, "第一个")
	if err != nil {
		t.Fatalf("第一个任务应能提交: %v", err)
	}
	waitRunning(t, a, first.ID)

	second, err := a.SubmitTask(ctx, sess.ID, "第二个")
	if err != nil {
		t.Fatalf("忙时提交应排队而不是被拒绝: %v", err)
	}
	if second.Status != domain.TaskQueued {
		t.Fatalf("第二个任务状态 = %s，期望 queued", second.Status)
	}
	// 会话内串行：第一个还在跑，第二个不能进 running。
	if got, err := a.GetTask(ctx, second.ID); err != nil || got.Status != domain.TaskQueued {
		t.Fatalf("排队任务不应提前启动: status=%v err=%v", got.Status, err)
	}

	// 第一个让出会话后，队首应当自动接班。
	if err := a.CancelTask(ctx, sess.ID, first.ID); err != nil {
		t.Fatalf("取消: %v", err)
	}
	waitTerminal(t, a, first.ID)
	waitRunning(t, a, second.ID)

	// 清理：取消后再关库，避免 Close 等满 5 秒。
	if err := a.CancelTask(ctx, sess.ID, second.ID); err != nil {
		t.Fatalf("取消第二个: %v", err)
	}
	waitTerminal(t, a, second.ID)
}

func TestCancelQueuedTask(t *testing.T) {
	ctx := context.Background()
	a := newApp(t) // 未配置 runtime：任务只入队

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "排队中")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CancelTask(ctx, sess.ID, task.ID); err != nil {
		t.Fatalf("取消: %v", err)
	}
	done, err := a.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.TaskCanceled {
		t.Fatalf("排队中的任务应直接取消，得到 %s", done.Status)
	}

	// 已终止的任务再取消是幂等的。
	if err := a.CancelTask(ctx, sess.ID, task.ID); err != nil {
		t.Fatalf("重复取消应无错: %v", err)
	}
}

func TestCancelRunningTaskStopsAgent(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	configureRuntime(t, a, &fakeProvider{block: true})

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "长任务")
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, a, task.ID)

	if err := a.CancelTask(ctx, sess.ID, task.ID); err != nil {
		t.Fatalf("取消运行中任务: %v", err)
	}
	done := waitTerminal(t, a, task.ID)
	if done.Status != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled", done.Status)
	}
}

func TestEventsReadsWholeHistory(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitTask(ctx, sess.ID, "一"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitTask(ctx, sess.ID, "二"); err != nil {
		t.Fatal(err)
	}
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) < 3 {
		t.Fatalf("事件数 = %d，期望至少 3（session.created + 两次 task.queued）", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("事件未按 seq 递增：%d 后是 %d", events[i-1].Seq, events[i].Seq)
		}
	}
}

func TestDeltaRejectsNonZeroSeq(t *testing.T) {
	a := newApp(t)
	ev := domain.Event{Type: domain.EventAssistantDelta, SessionID: "ses_1", Seq: 5}
	if err := a.Delta(context.Background(), ev); err == nil {
		t.Fatal("带 seq 的增量帧应被拒绝")
	}
}

// TestCancelImmediatelyAfterSubmit 覆盖「取消早于执行协程启动」的竞态：
// 任务已登记但还没进 running，取消仍然必须落到 canceled 终态，
// 否则它会永远占着会话，后续提交一直 session_busy。
func TestCancelImmediatelyAfterSubmit(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	configureRuntime(t, a, &fakeProvider{block: true})

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}

	task, err := a.SubmitTask(ctx, sess.ID, "立刻取消")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	// 不等待 running：这一刻协程可能还没开始执行。
	if err := a.CancelTask(ctx, sess.ID, task.ID); err != nil {
		t.Fatalf("取消: %v", err)
	}

	done := waitTerminal(t, a, task.ID)
	if done.Status != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled", done.Status)
	}

	// 会话必须重新可用：能再提交并正常跑完。
	next, err := a.SubmitTask(ctx, sess.ID, "第二个")
	if err != nil {
		t.Fatalf("取消后应能提交新任务: %v", err)
	}
	if err := a.CancelTask(ctx, sess.ID, next.ID); err != nil {
		t.Fatalf("取消第二个任务: %v", err)
	}
	if got := waitTerminal(t, a, next.ID); got.Status != domain.TaskCanceled {
		t.Fatalf("第二个任务终态 = %s，期望 canceled", got.Status)
	}
}

func TestCancelQueuedTaskWithCanceledContext(t *testing.T) {
	ctx := context.Background()
	a := newApp(t) // 未配置 runtime：任务只入队

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "排队中")
	if err != nil {
		t.Fatal(err)
	}

	// 客户端常在发完 task.cancel 后立刻断开：取消不能跟着连接一起失效。
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := a.CancelTask(canceledCtx, sess.ID, task.ID); err != nil {
		t.Fatalf("取消应成功，得到 %v", err)
	}
	done, err := a.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.TaskCanceled {
		t.Fatalf("任务状态 = %s，期望 canceled", done.Status)
	}
}
