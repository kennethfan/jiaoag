package store_test

import (
	"context"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

func TestStatsCountsSessionsTasksEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	sess := seedSession(t, s)
	queueTask(t, s, sess.ID, "tsk_stats")

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Sessions != 1 {
		t.Errorf("会话数应为 1，得到 %d", stats.Sessions)
	}
	if stats.TasksByStatus[string(domain.TaskQueued)] != 1 {
		t.Errorf("queued 任务数应为 1，得到 %v", stats.TasksByStatus)
	}
	if stats.Events < 1 {
		t.Errorf("事件数应至少 1，得到 %d", stats.Events)
	}
	if stats.ProcsRunning != 0 || stats.ProcsOrphaned != 0 {
		t.Errorf("没有后台进程时应为 0，得到 running=%d orphaned=%d", stats.ProcsRunning, stats.ProcsOrphaned)
	}
}

func TestStatsCountsBackgroundProcsByStatus(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	sess := seedSession(t, s)

	for _, p := range []store.BackgroundProc{
		{ID: "proc_run", SessionID: string(sess.ID), Owner: "rt_1", PID: 1, Status: store.ProcRunning, StartedAt: domain.Now()},
		{ID: "proc_orphan", SessionID: string(sess.ID), Owner: "rt_dead", PID: 2, Status: store.ProcOrphaned, StartedAt: domain.Now()},
		{ID: "proc_done", SessionID: string(sess.ID), Owner: "rt_1", PID: 3, Status: store.ProcExited, StartedAt: domain.Now()},
	} {
		if err := s.RegisterBackgroundProc(ctx, p); err != nil {
			t.Fatalf("RegisterBackgroundProc %s: %v", p.ID, err)
		}
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ProcsRunning != 1 || stats.ProcsOrphaned != 1 {
		t.Errorf("应各计 1，得到 running=%d orphaned=%d", stats.ProcsRunning, stats.ProcsOrphaned)
	}
}
