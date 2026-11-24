package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/store"
)

// busyApp 保留存储句柄：这几个用例要把任务推进到 waiting_approval，
// 而 App 只暴露了提交与查询。
func busyApp(t *testing.T) (*app.App, *store.Store) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })
	return a, db
}

func markWaitingApproval(t *testing.T, db *store.Store, taskID domain.TaskID) {
	t.Helper()
	if _, err := db.Apply(context.Background(), domain.Transition{
		TaskID: taskID,
		To:     domain.TaskWaitingApproval,
	}); err != nil {
		t.Fatalf("推进到 waiting_approval: %v", err)
	}
}

// 库里停在 queued、但内存里没人管的任务不能算「忙」。
//
// 这是空闲退出的判据：按库判断时，一条重启后没能重新入队的孤儿行会让服务端
// 永远等不到空闲，进程挂着跑过夜（真实踩到过：老二进制挂 4.5 小时）。
func TestHasBusyTasksIgnoresOrphanQueued(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	// 未配置运行时：任务只落库为 queued，不进入内存队列。
	task := submitTask(t, a, sess, "只落库")
	if got, err := a.GetTask(ctx, task.ID); err != nil || got.Status != domain.TaskQueued {
		t.Fatalf("任务应为 queued，得到 %v / %v", got.Status, err)
	}

	busy, err := a.HasBusyTasks(ctx)
	if err != nil {
		t.Fatalf("HasBusyTasks: %v", err)
	}
	if busy {
		t.Fatal("孤立的 queued 行不应被当成忙")
	}
}

// 真正在跑的任务必须算忙：空闲退出不能把正在干活的会话打断。
func TestHasBusyTasksSeesRunningTask(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	block := &stepProvider{release: make(chan struct{})}
	configureResume(t, a, block, false)

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	task := submitTask(t, a, sess, "跑起来")
	waitRunning(t, a, task.ID)

	busy, err := a.HasBusyTasks(ctx)
	if err != nil {
		t.Fatalf("HasBusyTasks: %v", err)
	}
	if !busy {
		t.Fatal("执行中的任务应算忙")
	}

	// 放行之后任务收尾，忙的判定要跟着回到空闲。
	close(block.release)
	if got := waitTerminal(t, a, task.ID); got.Status != domain.TaskCompleted {
		t.Fatalf("任务终态 = %s", got.Status)
	}
	busy, err = a.HasBusyTasks(ctx)
	if err != nil {
		t.Fatalf("HasBusyTasks: %v", err)
	}
	if busy {
		t.Fatal("任务结束后不应再算忙")
	}
}

// 等审批的任务不算忙：客户端已经全断，没人能裁决，等下去只会永不退出。
// 协程仍挂在 AwaitApproval 上，所以不能只看「有没有 run」。
func TestHasBusyTasksIgnoresWaitingApproval(t *testing.T) {
	ctx := context.Background()
	a, db := busyApp(t)
	block := &stepProvider{release: make(chan struct{})}
	defer close(block.release)
	configureResume(t, a, block, false)

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	task := submitTask(t, a, sess, "等审批")
	waitRunning(t, a, task.ID)
	markWaitingApproval(t, db, task.ID)

	busy, err := a.HasBusyTasks(ctx)
	if err != nil {
		t.Fatalf("HasBusyTasks: %v", err)
	}
	if busy {
		t.Fatal("等审批的任务不应阻止空闲退出")
	}
}

// 排在等审批会话后面的任务同样不算忙：前一个不裁决，它永远轮不到执行。
func TestHasBusyTasksIgnoresQueuedBehindApproval(t *testing.T) {
	ctx := context.Background()
	a, db := busyApp(t)
	block := &stepProvider{release: make(chan struct{})}
	defer close(block.release)
	configureResume(t, a, block, false)

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	first := submitTask(t, a, sess, "先跑")
	waitRunning(t, a, first.ID)
	submitTask(t, a, sess, "排队")

	busy, err := a.HasBusyTasks(ctx)
	if err != nil {
		t.Fatalf("HasBusyTasks: %v", err)
	}
	if !busy {
		t.Fatal("会话里有执行中任务时应算忙")
	}

	markWaitingApproval(t, db, first.ID)
	busy, err = a.HasBusyTasks(ctx)
	if err != nil {
		t.Fatalf("HasBusyTasks: %v", err)
	}
	if busy {
		t.Fatal("前面卡在等审批时，后面的排队任务也不该阻止退出")
	}
}
