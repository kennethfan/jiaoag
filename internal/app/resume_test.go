package app_test

import (
	"context"
	"errors"
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

// resumeProvider 让任务立刻成功结束，用于验证恢复后的排队任务真的跑完。
type resumeProvider struct{}

func (resumeProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (resumeProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if err := fn(llm.Chunk{TextDelta: "好了"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 1}})
}

func configureResume(t *testing.T, a *app.App, provider llm.Provider, resume bool) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:     3,
		ContextBudget: 10000,
		ResumeQueued:  resume,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

func lastTaskStatus(t *testing.T, a *app.App, sessionID domain.SessionID, taskID domain.TaskID) domain.TaskStatusPayload {
	t.Helper()
	events, err := a.Events(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("读取事件: %v", err)
	}
	var payload domain.TaskStatusPayload
	for _, ev := range events {
		if ev.TaskID != taskID || ev.Type != domain.EventTaskStatus {
			continue
		}
		if err := ev.Decode(&payload); err != nil {
			t.Fatalf("解码状态事件: %v", err)
		}
	}
	return payload
}

// 正常重启：执行中的任务 canceled，排队任务保留并在新进程里跑完。
func TestResumeQueuedTasksAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	block := &stepProvider{release: make(chan struct{})}
	configureResume(t, a, block, true)

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	first := submitTask(t, a, sess, "第一个")
	waitRunning(t, a, first.ID)
	second := submitTask(t, a, sess, "第二个")
	if task := waitStatus(t, a, second.ID, domain.TaskQueued); task.Status != domain.TaskQueued {
		t.Fatalf("第二个任务应排队，实际 %s", task.Status)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	reopened, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	saved, err := reopened.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatalf("读取排队任务: %v", err)
	}
	if saved.Status != domain.TaskQueued {
		t.Fatalf("开启续跑时关闭不应取消排队任务，实际 %s", saved.Status)
	}
	done, err := reopened.GetTask(ctx, first.ID)
	if err != nil {
		t.Fatalf("读取执行中任务: %v", err)
	}
	if done.Status != domain.TaskCanceled {
		t.Fatalf("执行中的任务仍应 canceled，实际 %s", done.Status)
	}

	b := app.New(reopened, event.NewHub())
	configureResume(t, b, resumeProvider{}, true)
	if task := waitTerminal(t, b, second.ID); task.Status != domain.TaskCompleted {
		t.Fatalf("恢复后的排队任务状态 = %s，期望 completed", task.Status)
	}
}

// 恢复失败（例如工作区已不存在）必须把任务标成 failed，不能永远停在 queued。
func TestResumeQueuedTaskFailureMarksFailed(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatalf("创建工作区: %v", err)
	}
	sess, err := a.CreateSession(ctx, gone, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	// 未配置运行时：任务只落库为 queued，不会进入内存队列。
	task := submitTask(t, a, sess, "留给下次启动")
	if err := a.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("删除工作区: %v", err)
	}

	reopened, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	b := app.New(reopened, event.NewHub())
	configureResume(t, b, resumeProvider{}, true)
	got := waitTerminal(t, b, task.ID)
	if got.Status != domain.TaskFailed {
		t.Fatalf("恢复失败的任务状态 = %s，期望 failed", got.Status)
	}
	payload := lastTaskStatus(t, b, sess.ID, task.ID)
	if payload.Reason != "resume_failed" || !strings.Contains(payload.Error, "重建执行环境失败") {
		t.Fatalf("恢复失败状态事件不完整：%+v", payload)
	}
}
