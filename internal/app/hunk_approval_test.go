package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
)

// TestAllowSessionWithHunksDowngradesToAllowOnce 确认「带块选择的 allow_session」
// 只对这一次生效：部分批准不能代表以后同类请求也都放行。
func TestAllowSessionWithHunksDowngradesToAllowOnce(t *testing.T) {
	ctx := context.Background()
	provider := &roundProvider{rounds: []func(fn func(llm.Chunk) error) error{
		writeToolRound("call_w1", "out.txt", "same content"),
		textRound("第一次完成"),
		writeToolRound("call_w2", "out.txt", "same content"),
		textRound("第二次完成"),
	}}
	a := newApp(t)
	configureWriteRuntime(t, a, provider)

	dir := t.TempDir()
	sess, err := a.CreateSession(ctx, dir, "p", "m", "部分批准")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, err := a.SubmitTask(ctx, sess.ID, "写一次")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approvalID := waitApproval(t, a, sess.ID)
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowSession, "", 0); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminal(t, a, first.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("第一个任务终态 = %s（%s）", done.Status, done.Error)
	}

	// 记录里应该已经降级成 allow_once，否则后续同样请求会被静默放行。
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var last *domain.ApprovalResolvedPayload
	for _, ev := range events {
		if ev.Type != domain.EventApprovalResolved {
			continue
		}
		var p domain.ApprovalResolvedPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		last = &p
	}
	if last == nil {
		t.Fatal("没有 approval.resolved")
	}
	if last.Decision != domain.DecisionAllowOnce {
		t.Fatalf("裁决 = %s，期望降级为 allow_once", last.Decision)
	}

	// 第二个任务参数完全相同：降级后必须重新弹审批，而不是命中白名单。
	// 先把文件改回去，否则同样内容会被当成「没有变化」直接报错。
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitTask(ctx, sess.ID, "再写一次"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	second := waitNewApproval(t, a, sess.ID, approvalID)
	// 收尾：拒绝掉，避免任务挂在那里。
	if err := a.ResolveApproval(ctx, sess.ID, second, domain.DecisionDeny, "收尾"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
}

// TestResolveApprovalRejectsBadHunks 确认块下标有问题时直接报错，不落裁决。
func TestResolveApprovalRejectsBadHunks(t *testing.T) {
	ctx := context.Background()
	provider := &roundProvider{rounds: []func(fn func(llm.Chunk) error) error{
		writeToolRound("call_w1", "out.txt", "content"),
		textRound("完成"),
	}}
	a := newApp(t)
	configureWriteRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, t.TempDir(), "p", "m", "坏下标")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := a.SubmitTask(ctx, sess.ID, "写一次"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approvalID := waitApproval(t, a, sess.ID)

	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, "", -1); err == nil {
		t.Fatal("负数块下标应该报错")
	}
	// 报错不应消耗掉这次审批：正常裁决仍然可用。
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, ""); err != nil {
		t.Fatalf("正常裁决失败: %v", err)
	}
}

// waitNewApproval 等到出现一条 id 不等于 exclude 的审批请求。
func waitNewApproval(t *testing.T, a *app.App, sessionID domain.SessionID, exclude domain.ApprovalID) domain.ApprovalID {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := a.Events(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("读取事件: %v", err)
		}
		for _, ev := range events {
			if ev.Type != domain.EventApprovalRequested {
				continue
			}
			var p domain.ApprovalRequestedPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatalf("解码审批请求: %v", err)
			}
			if p.ApprovalID != exclude {
				return p.ApprovalID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("没有等到新的 approval.requested")
	return ""
}
