package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// blockedProvider 卡住直到 release 关闭，用来让任务停在 running。
type blockedProvider struct{ release chan struct{} }

func (p *blockedProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *blockedProvider) ChatStream(ctx context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
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

// 租约过期后由接管方标记 failed(owner_lost)，绝不重新入队重跑。
func TestReapExpiredLeaseFailsTask(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if err := db.RegisterRuntime(ctx, store.Runtime{
		ID: "rt_1", PID: 1, Host: "test", Kind: "tui",
		StartedAt: time.Now(), HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatalf("RegisterRuntime: %v", err)
	}
	a := New(db, event.NewHub())
	defer a.Close()

	hold := &blockedProvider{release: make(chan struct{})}
	defer close(hold.release)
	if err := a.ConfigureRuntime(RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return hold, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:     2,
		ContextBudget: 10000,
		RuntimeID:     "rt_1",
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "租约")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "长任务")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 等它被占位并进入 running。
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, err := db.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if cur.Status == domain.TaskRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务没有进入 running：%+v", cur)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 模拟进程失联：把这个 owner 的租约改成过去，然后跑一次接管。
	if _, err := db.RenewTaskLeases(ctx, "rt_1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("RenewTaskLeases: %v", err)
	}
	// 接管者是另一个运行时：把自己的身份临时换掉，模拟「别人来收尸」。
	a.owner = "rt_2"
	a.reapExpiredLeases(ctx)
	a.owner = "rt_1"

	cur, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if cur.Status != domain.TaskFailed {
		t.Fatalf("过期租约应当标记 failed，实际 %s", cur.Status)
	}
	if cur.Error == "" || !contains(cur.Error, "owner_lost") {
		t.Fatalf("失败原因应当说明 owner_lost，实际 %q", cur.Error)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
