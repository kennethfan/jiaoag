package app_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/store"
)

// retentionApp 额外把库路径与句柄交给测试，方便把 updated_at 改成很久以前
// （候选查询只认时间，不认「更新时间是不是刚写的」）。
func retentionApp(t *testing.T) (*app.App, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })
	return a, db, path
}

// ageSession 把会话的最后活动时间改到 2020 年，模拟「很久没用过」。
func ageSession(t *testing.T, path string, id domain.SessionID) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开原始连接: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		"2020-01-01T00:00:00Z", string(id)); err != nil {
		t.Fatalf("改写 updated_at: %v", err)
	}
}

func TestPruneCandidatesSkipsActiveSessions(t *testing.T) {
	ctx := context.Background()
	a, db, path := retentionApp(t)

	old, err := a.CreateSession(ctx, t.TempDir(), "", "", "很久以前")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := a.CreateSession(ctx, t.TempDir(), "", "", "还有任务在跑")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := a.CreateSession(ctx, t.TempDir(), "", "", "刚建的")
	if err != nil {
		t.Fatal(err)
	}

	ev, err := domain.NewEvent(domain.EventTaskQueued, busy.ID, "", domain.TaskQueuedPayload{Prompt: "跑着"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, domain.Task{
		ID: "tsk_busy", SessionID: busy.ID, Status: domain.TaskQueued, Prompt: "跑着",
	}, []domain.Event{ev}); err != nil {
		t.Fatalf("种入排队任务: %v", err)
	}

	ageSession(t, path, old.ID)
	ageSession(t, path, busy.ID)

	cands, err := a.PruneCandidates(ctx, 90*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("应只列出 1 个过期且空闲的会话，实际 %d：%+v", len(cands), cands)
	}
	if cands[0].ID != string(old.ID) {
		t.Errorf("列出的应是 %s，实际 %s", old.ID, cands[0].ID)
	}
	if cands[0].Title != "很久以前" {
		t.Errorf("标题未透传: %+v", cands[0])
	}

	// 刚建的会话不该入选。
	for _, c := range cands {
		if c.ID == string(fresh.ID) {
			t.Errorf("刚建的会话不该被清理: %+v", c)
		}
	}
}

func TestPruneSessionsDeletesWholeSession(t *testing.T) {
	ctx := context.Background()
	a, db, path := retentionApp(t)

	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "待清理")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, "", domain.TaskQueuedPayload{Prompt: "旧任务"})
	if err != nil {
		t.Fatal(err)
	}
	// 直接写一条已结束的任务，避免提交真正跑起来。
	if _, err := db.CreateTask(ctx, domain.Task{
		ID: "tsk_old", SessionID: sess.ID, Status: domain.TaskRunning, Prompt: "旧任务",
	}, []domain.Event{queued}); err != nil {
		t.Fatalf("种入任务: %v", err)
	}
	if _, err := db.Apply(ctx, domain.Transition{TaskID: "tsk_old", To: domain.TaskCompleted}); err != nil {
		t.Fatalf("置为完成: %v", err)
	}
	ageSession(t, path, sess.ID)

	deleted, err := a.PruneSessions(ctx, []string{string(sess.ID)})
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("应删除 1 个会话，实际 %d", deleted)
	}

	if _, err := a.GetSession(ctx, sess.ID); err == nil {
		t.Error("会话应已被删除")
	}
	events, err := db.Replay(ctx, sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("事件应随会话一起删除，实际剩 %d 条", len(events))
	}
	summaries, err := a.SessionSummaries(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Errorf("会话列表应为空，实际 %d 条", len(summaries))
	}
}

func TestPruneSessionsReportsBusySession(t *testing.T) {
	ctx := context.Background()
	a, db, _ := retentionApp(t)

	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "忙")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, "", domain.TaskQueuedPayload{Prompt: "跑着"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, domain.Task{
		ID: "tsk_running", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "跑着",
	}, []domain.Event{ev}); err != nil {
		t.Fatal(err)
	}

	deleted, err := a.PruneSessions(ctx, []string{string(sess.ID)})
	if err == nil {
		t.Fatal("删除有未结束任务的会话应报错")
	}
	if deleted != 0 {
		t.Errorf("不应删除任何会话，实际 %d", deleted)
	}
	if _, err := a.GetSession(ctx, sess.ID); err != nil {
		t.Errorf("会话应仍在: %v", err)
	}
}
