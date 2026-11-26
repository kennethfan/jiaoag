package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

func makeSession(t *testing.T, db *store.Store, id string) domain.Session {
	t.Helper()
	sess := domain.Session{ID: domain.SessionID(id), Workspace: t.TempDir()}
	ev, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
		Workspace: sess.Workspace,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.CreateSession(context.Background(), sess, []domain.Event{ev}); err != nil {
		t.Fatalf("建会话: %v", err)
	}
	return sess
}

func TestBackgroundProcLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sess := makeSession(t, db, "ses_proc")

	started := time.Unix(1700000000, 0)
	err = db.RegisterBackgroundProc(ctx, store.BackgroundProc{
		ID: "call_1", SessionID: string(sess.ID), Owner: "rt_1", PID: 4242,
		OutputPath: "/tmp/out.log", StartedAt: started,
	})
	if err != nil {
		t.Fatalf("登记: %v", err)
	}

	list, err := db.ListBackgroundProcs(ctx, string(sess.ID))
	if err != nil {
		t.Fatalf("列表: %v", err)
	}
	if len(list) != 1 || list[0].Status != store.ProcRunning || list[0].PID != 4242 {
		t.Fatalf("列表 = %+v", list)
	}

	// 别人的运行时只能投递停止意图。
	if err := db.RequestBackgroundProcStop(ctx, string(sess.ID), "call_1"); err != nil {
		t.Fatalf("请求停止: %v", err)
	}
	if stop, err := db.BackgroundProcStopRequested(ctx, "call_1"); err != nil || !stop {
		t.Fatalf("停止标记 = %v, %v", stop, err)
	}

	if err := db.MarkBackgroundProc(ctx, "call_1", store.ProcStopped); err != nil {
		t.Fatalf("标记: %v", err)
	}
	list, _ = db.ListBackgroundProcs(ctx, string(sess.ID))
	if list[0].Status != store.ProcStopped {
		t.Fatalf("状态 = %q", list[0].Status)
	}

	// 新的请求不会再改已停止的进程。
	if err := db.RequestBackgroundProcStop(ctx, string(sess.ID), "call_1"); err != nil {
		t.Fatalf("重复请求: %v", err)
	}
	if stop, _ := db.BackgroundProcStopRequested(ctx, "call_1"); !stop {
		t.Fatal("停止标记应保留")
	}
}

func TestOrphanBackgroundProcs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sess := makeSession(t, db, "ses_orphan")
	for _, rec := range []store.BackgroundProc{
		{ID: "alive", SessionID: string(sess.ID), Owner: "rt_alive", StartedAt: time.Now()},
		{ID: "gone", SessionID: string(sess.ID), Owner: "rt_gone", StartedAt: time.Now()},
	} {
		if err := db.RegisterBackgroundProc(ctx, rec); err != nil {
			t.Fatalf("登记: %v", err)
		}
	}

	n, err := db.OrphanBackgroundProcs(ctx, []string{"rt_alive"})
	if err != nil {
		t.Fatalf("标记孤儿: %v", err)
	}
	if n != 1 {
		t.Fatalf("标记数 = %d，期望 1", n)
	}
	list, _ := db.ListBackgroundProcs(ctx, string(sess.ID))
	status := map[string]string{}
	for _, rec := range list {
		status[rec.ID] = rec.Status
	}
	if status["alive"] != store.ProcRunning || status["gone"] != store.ProcOrphaned {
		t.Fatalf("状态 = %+v", status)
	}
}
