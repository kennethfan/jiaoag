package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
)

// stepProvider 每收到一次 release 才让一个任务结束。
//
// 用它把「任务何时结束」从时间变成显式信号：排队用例不必靠 sleep 猜时序，
// 断言的是「A 没结束时 B/C 一定还在 queued」。
type stepProvider struct {
	release chan struct{}
}

func (p *stepProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *stepProvider) ChatStream(ctx context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	if err := fn(llm.Chunk{TextDelta: "好的"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

// panicProvider 第一次流式调用直接 panic，之后正常结束。
//
// 模拟供应商适配器在边界外崩溃：只能影响当前任务，不能带走服务端。
type panicProvider struct {
	mu   sync.Mutex
	used bool
}

func (p *panicProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *panicProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	p.mu.Lock()
	first := !p.used
	p.used = true
	p.mu.Unlock()
	if first {
		panic("测试用 provider 崩溃")
	}
	if err := fn(llm.Chunk{TextDelta: "好的"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

func submitTask(t *testing.T, a *app.App, sess domain.Session, prompt string) domain.Task {
	t.Helper()
	task, err := a.SubmitTask(context.Background(), sess.ID, prompt)
	if err != nil {
		t.Fatalf("提交任务 %q: %v", prompt, err)
	}
	return task
}

func waitStatus(t *testing.T, a *app.App, id domain.TaskID, want domain.TaskStatus) domain.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := a.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("读取任务 %s: %v", id, err)
		}
		if task.Status == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := a.GetTask(context.Background(), id)
	t.Fatalf("任务 %s 状态 = %s，期望 %s", id, task.Status, want)
	return domain.Task{}
}

// 会话内串行：A 未结束时 B/C 停在 queued，结束后严格按提交顺序拉起。
func TestSessionQueueRunsTasksInOrder(t *testing.T) {
	a := newApp(t)
	prov := &stepProvider{release: make(chan struct{})}
	configureRuntime(t, a, prov)

	sess, err := a.CreateSession(context.Background(), t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	first := submitTask(t, a, sess, "第一个")
	second := submitTask(t, a, sess, "第二个")
	third := submitTask(t, a, sess, "第三个")

	waitRunning(t, a, first.ID)
	for _, id := range []domain.TaskID{second.ID, third.ID} {
		if task := waitStatus(t, a, id, domain.TaskQueued); task.Status != domain.TaskQueued {
			t.Fatalf("任务 %s 应保持排队，实际 %s", id, task.Status)
		}
	}

	// 放行第一个：它结束后第二个被拉起，第三个仍排队。
	prov.release <- struct{}{}
	if task := waitTerminal(t, a, first.ID); task.Status != domain.TaskCompleted {
		t.Fatalf("第一个任务状态 = %s，期望 completed", task.Status)
	}
	waitRunning(t, a, second.ID)
	if task := waitStatus(t, a, third.ID, domain.TaskQueued); task.Status != domain.TaskQueued {
		t.Fatalf("第三个任务应仍在排队，实际 %s", task.Status)
	}

	prov.release <- struct{}{}
	if task := waitTerminal(t, a, second.ID); task.Status != domain.TaskCompleted {
		t.Fatalf("第二个任务状态 = %s，期望 completed", task.Status)
	}
	waitRunning(t, a, third.ID)

	prov.release <- struct{}{}
	if task := waitTerminal(t, a, third.ID); task.Status != domain.TaskCompleted {
		t.Fatalf("第三个任务状态 = %s，期望 completed", task.Status)
	}
}

// 排队中的任务被取消后不会在轮到它时开跑。
func TestCancelQueuedTaskNeverStarts(t *testing.T) {
	a := newApp(t)
	prov := &stepProvider{release: make(chan struct{})}
	configureRuntime(t, a, prov)

	sess, err := a.CreateSession(context.Background(), t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	first := submitTask(t, a, sess, "第一个")
	second := submitTask(t, a, sess, "第二个")
	waitRunning(t, a, first.ID)

	if err := a.CancelTask(context.Background(), sess.ID, second.ID); err != nil {
		t.Fatalf("取消排队任务: %v", err)
	}
	if task := waitStatus(t, a, second.ID, domain.TaskCanceled); task.Status != domain.TaskCanceled {
		t.Fatalf("排队任务应直接取消，实际 %s", task.Status)
	}
	if task, _ := a.GetTask(context.Background(), first.ID); task.Status != domain.TaskRunning {
		t.Fatalf("取消排队任务不应影响运行中的任务，实际 %s", task.Status)
	}

	prov.release <- struct{}{}
	waitTerminal(t, a, first.ID)

	// 已取消的任务不能被 startNextLocked 起来：第一个任务结束后，会话里
	// 不应出现新的 running 任务，且 second 仍是 canceled。
	tasks, err := a.ListTasks(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("列出任务: %v", err)
	}
	for _, task := range tasks {
		if task.Status == domain.TaskRunning {
			t.Fatalf("会话里不应再有 running 任务，实际 %s", task.ID)
		}
	}
	if task, err := a.GetTask(context.Background(), second.ID); err != nil || task.Status != domain.TaskCanceled {
		t.Fatalf("已取消的排队任务被启动了：%v %s", err, task.Status)
	}
}

// 取消运行中的任务后，队列里的下一个自动开跑。
func TestCancelRunningTaskStartsNext(t *testing.T) {
	a := newApp(t)
	prov := &stepProvider{release: make(chan struct{})}
	configureRuntime(t, a, prov)

	sess, err := a.CreateSession(context.Background(), t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	first := submitTask(t, a, sess, "第一个")
	second := submitTask(t, a, sess, "第二个")
	waitRunning(t, a, first.ID)

	if err := a.CancelTask(context.Background(), sess.ID, first.ID); err != nil {
		t.Fatalf("取消运行中任务: %v", err)
	}
	if task := waitTerminal(t, a, first.ID); task.Status != domain.TaskCanceled {
		t.Fatalf("第一个任务状态 = %s，期望 canceled", task.Status)
	}
	waitRunning(t, a, second.ID)

	prov.release <- struct{}{}
	if task := waitTerminal(t, a, second.ID); task.Status != domain.TaskCompleted {
		t.Fatalf("第二个任务状态 = %s，期望 completed", task.Status)
	}
}

// provider panic 只让当前任务失败，队列里的下一个照常执行。
func TestProviderPanicFailsTaskAndNextRuns(t *testing.T) {
	a := newApp(t)
	configureRuntime(t, a, &panicProvider{})

	sess, err := a.CreateSession(context.Background(), t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	first := submitTask(t, a, sess, "会崩的任务")
	second := submitTask(t, a, sess, "正常任务")
	// panic 发生在 markRunning 之后、几毫秒内就落到 failed；用轮询去抓 running
	// 会与实现赛跑（CI 上稳定错过，本地偶发）。这里直接等终态，语义不变。
	failed := waitTerminal(t, a, first.ID)
	if failed.Status != domain.TaskFailed {
		t.Fatalf("panic 任务状态 = %s，期望 failed", failed.Status)
	}
	if failed.Error == "" {
		t.Fatal("panic 任务缺少错误信息")
	}

	// 下一个任务仍会被拉起并完成：panic 没有把会话占死，也没有带崩 App。
	if task := waitTerminal(t, a, second.ID); task.Status != domain.TaskCompleted {
		t.Fatalf("第二个任务状态 = %s，期望 completed", task.Status)
	}
}

func sessionCreatedPayload(t *testing.T, a *app.App, id domain.SessionID) domain.SessionCreatedPayload {
	t.Helper()
	events, err := a.Events(context.Background(), id)
	if err != nil {
		t.Fatalf("读取事件: %v", err)
	}
	for _, ev := range events {
		if ev.Type != domain.EventSessionCreated {
			continue
		}
		var p domain.SessionCreatedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("解析 session.created: %v", err)
		}
		return p
	}
	t.Fatalf("会话 %s 没有 session.created 事件", id)
	return domain.SessionCreatedPayload{}
}

// 同仓只警告不拒绝：会话 B 能看到「还有会话 A 在同一仓库有未完成的任务」。
func TestCreateSessionWarnsSharedWorkspace(t *testing.T) {
	a := newApp(t) // 未配置执行：任务停在 queued，仍算活跃
	ws := t.TempDir()

	first, err := a.CreateSession(context.Background(), ws, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	submitTask(t, a, first, "占住会话")

	second, err := a.CreateSession(context.Background(), ws, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("同仓建会话应成功: %v", err)
	}
	if p := sessionCreatedPayload(t, a, second.ID); !containsID(p.SharedWorkspace, string(first.ID)) {
		t.Fatalf("同仓警告缺少 %s：%v", first.ID, p.SharedWorkspace)
	}

	// 软链写法指向同一目录也必须命中。
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(ws, link); err != nil {
		t.Fatalf("创建软链: %v", err)
	}
	third, err := a.CreateSession(context.Background(), link, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("软链目录建会话: %v", err)
	}
	if p := sessionCreatedPayload(t, a, third.ID); !containsID(p.SharedWorkspace, string(first.ID)) {
		t.Fatalf("软链未被归一，缺少 %s：%v", first.ID, p.SharedWorkspace)
	}

	// 不同目录不提示。
	other, err := a.CreateSession(context.Background(), t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if p := sessionCreatedPayload(t, a, other.ID); len(p.SharedWorkspace) != 0 {
		t.Fatalf("不同目录不应提示，实际 %v", p.SharedWorkspace)
	}

	// 第一个会话的未完成任务收尾后，不再是活跃会话。
	events, err := a.Events(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("读取事件: %v", err)
	}
	var taskID domain.TaskID
	for _, ev := range events {
		if ev.Type == domain.EventTaskQueued {
			taskID = ev.TaskID
		}
	}
	if taskID == "" {
		t.Fatal("没找到第一个会话的任务")
	}
	if err := a.CancelTask(context.Background(), first.ID, taskID); err != nil {
		t.Fatalf("取消任务: %v", err)
	}
	last, err := a.CreateSession(context.Background(), ws, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if p := sessionCreatedPayload(t, a, last.ID); len(p.SharedWorkspace) != 0 {
		t.Fatalf("任务都结束后不应提示，实际 %v", p.SharedWorkspace)
	}
}

// 关闭时排队任务必须落终态，不能留给下个进程。
func TestCloseCancelsQueuedTasks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	// 第一个任务真的跑起来（阻塞等待 release），第二个才进内存队列：
	// 关闭路径要同时收尾 running 与 queued 两种状态。
	configureRuntime(t, a, &stepProvider{release: make(chan struct{})})

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	first := submitTask(t, a, sess, "第一个")
	waitRunning(t, a, first.ID)
	second := submitTask(t, a, sess, "第二个")

	if err := a.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	for _, id := range []domain.TaskID{first.ID, second.ID} {
		task, err := reopened.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("读取任务 %s: %v", id, err)
		}
		if task.Status != domain.TaskCanceled {
			t.Fatalf("关闭后任务 %s 状态 = %s，期望 canceled", id, task.Status)
		}
	}
}

func containsID(list []string, want string) bool {
	for _, id := range list {
		if id == want {
			return true
		}
	}
	return false
}
