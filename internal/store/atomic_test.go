package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// TestApplyIsAtomic 验证「事件与投影同事务」：提交前失败必须什么都不留。
// 这是白盒测试，靠包内钩子注入失败点。
func TestApplyIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer s.Close()

	sess := domain.Session{ID: "ses_atomic", Workspace: "/tmp/atomic"}
	created, _ := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{Workspace: sess.Workspace})
	if _, err := s.CreateSession(ctx, sess, []domain.Event{created}); err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	task := domain.Task{ID: "tsk_atomic", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "x"}
	queued, _ := domain.NewEvent(domain.EventTaskQueued, sess.ID, task.ID, domain.TaskQueuedPayload{Prompt: task.Prompt})
	if _, err := s.CreateTask(ctx, task, []domain.Event{queued}); err != nil {
		t.Fatalf("创建任务: %v", err)
	}

	s.hookBeforeCommit = func() error { return errors.New("注入失败") }
	ev, _ := domain.NewEvent(domain.EventTaskStatus, sess.ID, task.ID,
		domain.TaskStatusPayload{From: domain.TaskQueued, To: domain.TaskRunning})
	_, err = s.Apply(ctx, domain.Transition{
		TaskID: task.ID,
		To:     domain.TaskRunning,
		Events: []domain.Event{ev},
		Audit:  []domain.AuditRecord{{Actor: "test", Action: "task.transition"}},
	})
	s.hookBeforeCommit = nil
	if err == nil {
		t.Fatal("注入失败时 Apply 应返回错误")
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if got.Status != domain.TaskQueued {
		t.Fatalf("事务未回滚：状态变成 %s", got.Status)
	}
	if !got.StartedAt.IsZero() {
		t.Fatal("事务未回滚：started_at 被写入")
	}
	last, err := s.LastSeq(ctx, sess.ID)
	if err != nil {
		t.Fatalf("读取序号: %v", err)
	}
	if last != 2 {
		t.Fatalf("事务未回滚：事件已落库，LastSeq = %d", last)
	}

	var audits int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&audits); err != nil {
		t.Fatalf("统计审计: %v", err)
	}
	if audits != 0 {
		t.Fatalf("事务未回滚：审计留下 %d 条", audits)
	}
}
