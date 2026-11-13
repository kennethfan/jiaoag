package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// stuckProvider 无视 ctx 一直阻塞，用来模拟关闭窗口用完还没收尾的任务。
//
// entered 在真正进入流式调用时关闭：测试必须等到它。否则协程可能还没跑到第一
// 轮就看见取消、自己写下 canceled（合法终态），补写分支就永远不会被执行。
type stuckProvider struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (p *stuckProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *stuckProvider) ChatStream(context.Context, llm.Request, func(llm.Chunk) error) error {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return errors.New("测试结束")
}

// instantProvider 直接给一段文本，用来得到正常完成的任务。
type instantProvider struct{}

func (instantProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (instantProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if err := fn(llm.Chunk{TextDelta: "好的"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

// newTestApp 建一个接上执行依赖的 App，返回它与库路径。
func newTestApp(t *testing.T, provider llm.Provider) (*App, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := New(db, event.NewHub())
	if err := a.ConfigureRuntime(RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:     3,
		ContextBudget: 20000,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	return a, path
}

// 任务在关闭窗口内没收尾时，Close 必须替它写终态：否则关库之后协程再也写不
// 进去，库里会留下一条永远 running 的任务，重启后也永远占着会话。
func TestCloseWritesTerminalForUnfinishedTask(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	provider := &stuckProvider{release: release, entered: make(chan struct{})}
	a, path := newTestApp(t, provider)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "一直不返回的任务")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	waitTaskStatus(t, a, task.ID, domain.TaskRunning)
	// 等协程真的进到供应商调用里：它从此不会自己写终态，补写才是唯一出路。
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("任务未进入供应商调用")
	}

	// 窗口短到任务不可能收尾，走的就是「补写终态」这条分支。
	if err := a.close(50 * time.Millisecond); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	// 重复关闭必须立刻返回：库已关，既不该再等一轮窗口，也不该再碰它。
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("重复关闭: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("重复关闭耗时 %s，不应重新等待关闭窗口", elapsed)
	}

	// 关库之后重新打开，确认库里没有停在 running，且终态来自关闭流程。
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	defer reopened.Close()
	stored, err := reopened.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if stored.Status != domain.TaskCanceled {
		t.Fatalf("关闭后任务状态 = %s，期望 canceled", stored.Status)
	}
	events, err := reopened.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	statuses := statusPayloads(t, events)
	if len(statuses) == 0 {
		t.Fatal("缺少 task.status 事件")
	}
	last := statuses[len(statuses)-1]
	if last.From != domain.TaskRunning || last.To != domain.TaskCanceled || last.Reason != "shutdown" {
		t.Fatalf("补写的终态事件 = %+v，期望 running→canceled（reason=shutdown）", last)
	}
}

// 正常收尾的任务不能被关闭流程改写：补写只针对窗口用完还活着的任务。
func TestCloseKeepsFinishedTaskTerminal(t *testing.T) {
	ctx := context.Background()
	a, path := newTestApp(t, instantProvider{})
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "普通任务")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	waitTaskStatus(t, a, task.ID, domain.TaskCompleted)

	// 窗口取 0：不等，任何仍登记着的 run 都会进补写路径。
	if err := a.close(0); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	defer reopened.Close()
	stored, err := reopened.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if stored.Status != domain.TaskCompleted {
		t.Fatalf("已完成的任务被改成 %s", stored.Status)
	}
	events, err := reopened.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	for _, st := range statusPayloads(t, events) {
		if st.To == domain.TaskCanceled {
			t.Fatalf("已完成的任务不该被补写成 canceled：%+v", st)
		}
	}
}

// 补写的两个跳过分支要能确定性覆盖：done 已关（协程已收尾）与已有终态。
// 没有执行依赖的 App 让任务停在 queued，可以直接驱动 abandonRuns。
func TestAbandonRunsSkipBranches(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "queued.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := New(db, event.NewHub())
	defer a.Close()

	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "只入队")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	if got := taskStatus(t, a, task.ID); got != domain.TaskQueued {
		t.Fatalf("未配置执行时状态 = %s，期望 queued", got)
	}

	settled := make(chan struct{})
	close(settled)
	a.abandonRuns([]leftoverRun{{taskID: task.ID, done: settled}})
	if got := taskStatus(t, a, task.ID); got != domain.TaskQueued {
		t.Fatalf("done 已关时不该补写，状态 = %s", got)
	}

	a.abandonRuns([]leftoverRun{{taskID: task.ID, done: make(chan struct{})}})
	if got := taskStatus(t, a, task.ID); got != domain.TaskCanceled {
		t.Fatalf("应补写 canceled，状态 = %s", got)
	}
	before := len(statusPayloads(t, mustEvents(t, a, sess.ID)))

	a.abandonRuns([]leftoverRun{{taskID: task.ID, done: make(chan struct{})}})
	if after := len(statusPayloads(t, mustEvents(t, a, sess.ID))); after != before {
		t.Fatalf("终态任务不该再补写：task.status 事件 %d → %d", before, after)
	}
}

func taskStatus(t *testing.T, a *App, id domain.TaskID) domain.TaskStatus {
	t.Helper()
	task, err := a.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	return task.Status
}

func mustEvents(t *testing.T, a *App, sid domain.SessionID) []domain.Event {
	t.Helper()
	events, err := a.Events(context.Background(), sid)
	if err != nil {
		t.Fatalf("读取事件: %v", err)
	}
	return events
}

func statusPayloads(t *testing.T, events []domain.Event) []domain.TaskStatusPayload {
	t.Helper()
	var out []domain.TaskStatusPayload
	for _, ev := range events {
		if ev.Type != domain.EventTaskStatus {
			continue
		}
		var p domain.TaskStatusPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatalf("解码 task.status: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func waitTaskStatus(t *testing.T, a *App, id domain.TaskID, want domain.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := a.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("读取任务: %v", err)
		}
		if task.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务未在期限内进入 %s", want)
}
