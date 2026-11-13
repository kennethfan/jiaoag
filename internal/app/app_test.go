package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/store"
)

func newApp(t *testing.T) *app.App {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestCreateSessionWritesAndPublishes(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)

	sub := a.Hub().Subscribe("unused")
	sub.Close()

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "测试会话")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if !sess.ID.Valid() {
		t.Fatal("会话缺少 id")
	}

	events, err := a.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	if len(events) != 1 || events[0].Type != domain.EventSessionCreated {
		t.Fatalf("事件不符: %+v", events)
	}
	var p domain.SessionCreatedPayload
	if err := events[0].Decode(&p); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if p.Workspace != sess.Workspace || p.Title != "测试会话" {
		t.Fatalf("载荷不符: %+v", p)
	}
}

func TestSubmitTaskWritesQueuedEvent(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	sub := a.Hub().Subscribe(sess.ID)
	defer sub.Close()

	task, err := a.SubmitTask(ctx, sess.ID, "给 X 函数加测试")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	if task.Status != domain.TaskQueued {
		t.Fatalf("状态 = %s，期望 queued", task.Status)
	}

	select {
	case ev := <-sub.C():
		if ev.Type != domain.EventTaskQueued || ev.Seq != 2 {
			t.Fatalf("收到的事件不符: %s#%d", ev.Type, ev.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到 task.queued 广播")
	}

	stored, err := a.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if stored.Prompt != "给 X 函数加测试" || stored.LastSeq != 2 {
		t.Fatalf("落库结果不符: %+v", stored)
	}
}

func TestSubmitToUnknownSession(t *testing.T) {
	a := newApp(t)
	_, err := a.SubmitTask(context.Background(), "ses_missing", "x")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得到 %v", err)
	}
}

func TestCreateSessionRejectsRelativePath(t *testing.T) {
	a := newApp(t)
	_, err := a.CreateSession(context.Background(), "relative/path", "", "", "")
	if err == nil {
		t.Fatal("相对路径应被拒绝")
	}
}

func TestSubmitRejectsEmptyPrompt(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if _, err := a.SubmitTask(ctx, sess.ID, "   "); err == nil {
		t.Fatal("空任务应被拒绝")
	}
}

// 关闭之后所有写入都必须被明确拒绝：否则会落一条永远排不上、也没有终态的
// 任务，或者把 sqlite 的「数据库已关闭」错误当成业务错误抛给客户端。
// 这里自己建 App 而不是用 newApp，是为了只关一次。
func TestWritesAfterCloseAreRejected(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	task, err := a.SubmitTask(ctx, sess.ID, "关闭之后提交")
	if !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("关闭后提交应返回 ErrAppClosed，得到 %v", err)
	}
	if task.ID != "" {
		t.Fatalf("被拒绝的提交不应返回任务: %+v", task)
	}

	if _, err := a.CreateSession(ctx, t.TempDir(), "", "", ""); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("关闭后建会话应返回 ErrAppClosed，得到 %v", err)
	}
	if err := a.CancelTask(ctx, sess.ID, "tsk_missing"); !errors.Is(err, app.ErrAppClosed) {
		t.Fatalf("关闭后取消应返回 ErrAppClosed，得到 %v", err)
	}
}
