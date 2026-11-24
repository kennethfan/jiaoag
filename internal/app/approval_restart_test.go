package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
)

// TestAllowSessionSurvivesRestart 确认 allow_session 的授权落了库：
// 服务端重启后，同一会话里同样的命令不该再问一遍。
//
// 这条与「只在进程内」的行为差别正是 A4 的目的：重启不该把用户已经
// 表过态的事情再问一次，但授权带过期时间，不会变成永久豁免。
func TestAllowSessionSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "jiaoag.db")
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"cat", "-u", "notes.txt"}

	open := func() *app.App {
		db, err := store.Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("打开数据库: %v", err)
		}
		return app.New(db, event.NewHub())
	}

	// ——— 第一次进程：跑一次命令，用户选 allow_session ———
	a1 := open()
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_restart1", argv),
		textRound("第一次看过"),
	}}
	configureFullRuntime(t, a1, provider)
	sess, err := a1.CreateSession(ctx, ws, "fake", "test-model", "重启续用")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, err := a1.SubmitTask(ctx, sess.ID, "看看 notes.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approvalID := waitApproval(t, a1, sess.ID)
	if err := a1.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowSession, "以后不用问"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminalIn(t, a1, first.ID, 15*time.Second); done.Status != domain.TaskCompleted {
		t.Fatalf("第一个任务终态 = %s（%s）", done.Status, done.Error)
	}
	if err := a1.Close(); err != nil {
		t.Fatalf("关闭第一个 App: %v", err)
	}

	// ——— 第二次进程：同一会话、同一命令，不再需要人工裁决 ———
	a2 := open()
	t.Cleanup(func() { _ = a2.Close() })
	provider2 := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_restart2", argv),
		textRound("第二次没再问"),
	}}
	configureFullRuntime(t, a2, provider2)
	second, err := a2.SubmitTask(ctx, sess.ID, "再看一眼 notes.txt")
	if err != nil {
		t.Fatalf("SubmitTask（重启后）: %v", err)
	}
	// 没有 ResolveApproval：只有命中库里的授权才会自己走完。
	if done := waitTerminalIn(t, a2, second.ID, 15*time.Second); done.Status != domain.TaskCompleted {
		t.Fatalf("重启后任务终态 = %s（%s），像是又弹了一次审批", done.Status, done.Error)
	}

	events, err := a2.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 审批行与 approval.requested 事件照样会有：放行也是一次决定，审计不能缺。
	// 区别在裁决来自库里的授权（source=policy），而不是又等人按了一次 y。
	var auto int
	for _, ev := range events {
		if ev.Type != domain.EventApprovalResolved || ev.TaskID != second.ID {
			continue
		}
		var p domain.ApprovalResolvedPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatalf("解析 approval.resolved: %v", err)
		}
		if p.Source == domain.SourcePolicy {
			auto++
		}
	}
	if auto == 0 {
		t.Fatal("重启后的审批应带 source=policy（命中库里的 allow_session 授权）")
	}
}
