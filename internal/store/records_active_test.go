package store_test

import (
	"context"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

func activeSessionIDs(t *testing.T, s *store.Store) map[string]bool {
	t.Helper()
	sessions, err := s.SessionsWithActiveTasks(context.Background())
	if err != nil {
		t.Fatalf("列出活跃会话: %v", err)
	}
	out := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		out[string(sess.ID)] = true
	}
	return out
}

// 同仓检测依赖「未终态」这个口径：queued / running / waiting_approval 都算活跃。
func TestSessionsWithActiveTasksCoversUnfinishedStatuses(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)

	if !activeSessionIDs(t, s)[string(sess.ID)] {
		t.Fatal("queued 任务应让会话算作活跃")
	}

	apply := func(to domain.TaskStatus) {
		t.Helper()
		if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, To: to}); err != nil {
			t.Fatalf("迁移到 %s: %v", to, err)
		}
	}
	apply(domain.TaskRunning)
	if !activeSessionIDs(t, s)[string(sess.ID)] {
		t.Fatal("running 任务应让会话算作活跃")
	}
	apply(domain.TaskWaitingApproval)
	if !activeSessionIDs(t, s)[string(sess.ID)] {
		t.Fatal("waiting_approval 任务应让会话算作活跃")
	}
	apply(domain.TaskCanceled)
	if activeSessionIDs(t, s)[string(sess.ID)] {
		t.Fatal("终态任务不应让会话算作活跃")
	}

	// 完全没有任务的会话也不在结果里。
	other := domain.Session{ID: "ses_idle", Workspace: "/tmp/idle"}
	if _, err := s.CreateSession(ctx, other, nil); err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if activeSessionIDs(t, s)[string(other.ID)] {
		t.Fatal("没有任务的会话不应算作活跃")
	}
}
