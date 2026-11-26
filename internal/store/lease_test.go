package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

// queueTask 造一个排队中的任务，返回它的 id。
func queueTask(t *testing.T, s *store.Store, session domain.SessionID, id domain.TaskID) {
	t.Helper()
	if _, err := s.CreateTask(context.Background(), domain.Task{
		ID:        id,
		SessionID: session,
		Status:    domain.TaskQueued,
		Prompt:    "占位测试",
	}, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
}

func TestClaimTaskIsExclusive(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	sess := seedSession(t, s)
	queueTask(t, s, sess.ID, "tsk_a")

	until := time.Now().Add(time.Minute)
	ok, err := s.ClaimTask(ctx, "tsk_a", "rt_1", until, 4)
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if !ok {
		t.Fatalf("第一次占位应当成功")
	}

	// 别的运行时再抢同一个任务：status 已不是 queued，必须失败。
	if ok, err := s.ClaimTask(ctx, "tsk_a", "rt_2", until, 4); err != nil || ok {
		t.Fatalf("第二次占位应当失败，ok=%v err=%v", ok, err)
	}
	owner, err := s.TaskOwner(ctx, "tsk_a")
	if err != nil {
		t.Fatalf("TaskOwner: %v", err)
	}
	if owner != "rt_1" {
		t.Fatalf("owner 应当保持 rt_1，实际 %q", owner)
	}
}

// 同一会话串行：前一个任务在跑时，后面的排队任务不能被别的运行时抢走。
func TestClaimTaskKeepsSessionSerial(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	sess := seedSession(t, s)
	other := domain.Session{ID: "ses_other", Workspace: "/tmp/other"}
	if _, err := s.CreateSession(ctx, other, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	queueTask(t, s, sess.ID, "tsk_a")
	queueTask(t, s, sess.ID, "tsk_b")
	queueTask(t, s, other.ID, "tsk_c")

	until := time.Now().Add(time.Minute)
	if ok, err := s.ClaimTask(ctx, "tsk_a", "rt_1", until, 4); err != nil || !ok {
		t.Fatalf("占位 tsk_a: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimTask(ctx, "tsk_b", "rt_1", until, 4); err != nil {
		t.Fatalf("占位 tsk_b: %v", err)
	} else if ok {
		t.Fatalf("同会话已有执行中的任务，tsk_b 不该被占位")
	}
	if ok, err := s.ClaimTask(ctx, "tsk_c", "rt_1", until, 4); err != nil || !ok {
		t.Fatalf("别的会话应当能占位: ok=%v err=%v", ok, err)
	}
}

func TestClaimTaskHonoursGlobalLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	a := seedSession(t, s)
	b := domain.Session{ID: "ses_b", Workspace: "/tmp/b"}
	if _, err := s.CreateSession(ctx, b, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	queueTask(t, s, a.ID, "tsk_a")
	queueTask(t, s, b.ID, "tsk_b")

	until := time.Now().Add(time.Minute)
	if ok, err := s.ClaimTask(ctx, "tsk_a", "rt_1", until, 1); err != nil || !ok {
		t.Fatalf("占位第一个: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimTask(ctx, "tsk_b", "rt_1", until, 1); err != nil {
		t.Fatalf("占位第二个: %v", err)
	} else if ok {
		t.Fatalf("全局上限 1 时不该再占位")
	}
	// 上限为 0 表示不限，但第一个已占位不能重复占；换一个任务验证不受限。
	queueTask(t, s, b.ID, "tsk_b2")
	if ok, err := s.ClaimTask(ctx, "tsk_b2", "rt_1", until, 0); err != nil || !ok {
		t.Fatalf("不限上限应当能占位: ok=%v err=%v", ok, err)
	}
}

func TestLeaseRenewAndExpiry(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	sess := seedSession(t, s)
	queueTask(t, s, sess.ID, "tsk_a")

	past := time.Now().Add(-time.Minute)
	if ok, err := s.ClaimTask(ctx, "tsk_a", "rt_1", past, 4); err != nil || !ok {
		t.Fatalf("占位: ok=%v err=%v", ok, err)
	}
	expired, err := s.ExpiredTasks(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpiredTasks: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "tsk_a" || expired[0].Owner != "rt_1" {
		t.Fatalf("应当看到一条过期任务，实际 %+v", expired)
	}

	future := time.Now().Add(time.Minute)
	if _, err := s.RenewTaskLeases(ctx, "rt_1", future); err != nil {
		t.Fatalf("RenewTaskLeases: %v", err)
	}
	expired, err = s.ExpiredTasks(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpiredTasks: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("续租后不应再有过期任务：%+v", expired)
	}
	// 别的运行时续租不该碰到这条任务。
	if _, err := s.RenewTaskLeases(ctx, "rt_2", past); err != nil {
		t.Fatalf("RenewTaskLeases(rt_2): %v", err)
	}
	expired, err = s.ExpiredTasks(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpiredTasks: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("别的运行时不应改变这条任务的租约：%+v", expired)
	}
}

func TestRuntimeRegistry(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	now := time.Now()
	for _, rt := range []store.Runtime{
		{ID: "rt_1", PID: 1, Host: "h", Kind: "tui", StartedAt: now, HeartbeatAt: now},
		{ID: "rt_2", PID: 2, Host: "h", Kind: "serve", StartedAt: now, HeartbeatAt: now.Add(-time.Hour)},
	} {
		if err := s.RegisterRuntime(ctx, rt); err != nil {
			t.Fatalf("RegisterRuntime: %v", err)
		}
	}
	if err := s.HeartbeatRuntime(ctx, "rt_1", now); err != nil {
		t.Fatalf("HeartbeatRuntime: %v", err)
	}
	live, err := s.LiveRuntimes(ctx, now.Add(-30*time.Second))
	if err != nil {
		t.Fatalf("LiveRuntimes: %v", err)
	}
	if len(live) != 1 || live[0].ID != "rt_1" {
		t.Fatalf("应当只有 rt_1 存活，实际 %+v", live)
	}
	if err := s.RemoveRuntime(ctx, "rt_1"); err != nil {
		t.Fatalf("RemoveRuntime: %v", err)
	}
	live, err = s.LiveRuntimes(ctx, now.Add(-30*time.Second))
	if err != nil {
		t.Fatalf("LiveRuntimes: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("注销后不应有存活运行时：%+v", live)
	}
}
