package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// TestConcurrentSubmitKeepsBroadcastOrder 锁住「提交顺序 = 广播顺序」：
// 并发提交时，订阅者收到的 seq 必须严格递增，不能出现先到 6 后到 5。
func TestConcurrentSubmitKeepsBroadcastOrder(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	sub := a.Hub().Subscribe(sess.ID)
	defer sub.Close()

	const n = 20
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := a.SubmitTask(ctx, sess.ID, "任务")
			done <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	var last domain.Seq
	for i := 0; i < n; i++ {
		select {
		case ev := <-sub.C():
			if ev.Seq <= last {
				t.Fatalf("广播乱序: seq %d 出现在 %d 之后", ev.Seq, last)
			}
			last = ev.Seq
		case <-time.After(2 * time.Second):
			t.Fatalf("只收到 %d 条事件", i)
		}
	}
}
