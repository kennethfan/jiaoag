package app_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// TestCommitNotifiesObserver 确认旁路通知真的接在提交流程上：
// 审批请求与终态都要经过 Notify，且通知失败/未配置都不会影响任务。
func TestCommitNotifiesObserver(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_notify1", []string{"cat", "-u", "notes.txt"}),
		textRound("好"),
	}}

	var mu sync.Mutex
	var kinds []domain.EventKind
	err = a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(
				tools.NewReadFile(ws), tools.NewWriteFile(ws),
				tools.NewRunCommand(ws, env), tools.NewCheckCommand(ws, env),
			)
		},
		Backend:       execpkg.NewDirect(),
		OutputDir:     filepath.Join(t.TempDir(), "outputs"),
		Ruleset:       perm.DefaultRuleset(),
		ExecTimeout:   30 * time.Second,
		MaxRounds:     4,
		ContextBudget: 20000,
		Notify: func(ev domain.Event) {
			mu.Lock()
			kinds = append(kinds, ev.Type)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	sess, err := a.CreateSession(ctx, t.TempDir(), "fake", "test-model", "通知")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "看一眼 notes.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approvalID := waitApproval(t, a, sess.ID)
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionDeny, "不需要"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminalIn(t, a, task.ID, 15*time.Second); done.Status != domain.TaskCompleted {
		t.Fatalf("任务终态 = %s（%s）", done.Status, done.Error)
	}

	mu.Lock()
	seen := append([]domain.EventKind(nil), kinds...)
	mu.Unlock()
	has := func(kind domain.EventKind) bool {
		for _, k := range seen {
			if k == kind {
				return true
			}
		}
		return false
	}
	if !has(domain.EventApprovalRequested) {
		t.Fatalf("通知回调应收到 approval.requested，实际 %v", seen)
	}
	if !has(domain.EventTaskStatus) {
		t.Fatalf("通知回调应收到 task.status，实际 %v", seen)
	}
}
