package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/store"
)

func openApprovalStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// allow_session 的授权要跨重启存活，但必须有期限：它是一次便利，
// 不是一句「以后都别问我」的永久豁免。
func TestSessionApprovalLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openApprovalStore(t, filepath.Join(t.TempDir(), "jiaoag.db"))
	now := time.Now()

	ok, err := st.SessionApproval(ctx, "ses_1", "key-a", now)
	if err != nil {
		t.Fatalf("查询空表: %v", err)
	}
	if ok {
		t.Fatal("空表居然报告已授权")
	}

	if err := st.RememberSessionApproval(ctx, "ses_1", "key-a", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("记录授权: %v", err)
	}
	if ok, err = st.SessionApproval(ctx, "ses_1", "key-a", now); err != nil || !ok {
		t.Fatalf("刚写入的授权应命中，得到 ok=%v err=%v", ok, err)
	}

	// 会话与键都要匹配：另一条同参数调用落在别的会话里不该被放行。
	if ok, _ = st.SessionApproval(ctx, "ses_2", "key-a", now); ok {
		t.Fatal("跨会话命中了授权")
	}
	if ok, _ = st.SessionApproval(ctx, "ses_1", "key-b", now); ok {
		t.Fatal("不同参数命中了授权")
	}

	// 过期判定不依赖后台清理：读的时候按 expires_at 比。
	if ok, _ = st.SessionApproval(ctx, "ses_1", "key-a", now.Add(2*time.Hour)); ok {
		t.Fatal("已过期的授权仍在生效")
	}
	if err := st.RememberSessionApproval(ctx, "ses_1", "key-a", now, now); err == nil {
		t.Fatal("过期时间不晚于当前时间时应报错")
	}
}

func TestRememberSessionApprovalOverwritesAndPrunes(t *testing.T) {
	ctx := context.Background()
	st := openApprovalStore(t, filepath.Join(t.TempDir(), "jiaoag.db"))
	now := time.Now()

	// 一条已过期的旧记录，写入新授权时应该被顺手清掉。
	if err := st.RememberSessionApproval(ctx, "ses_old", "key-old", now.Add(-3*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("记录旧授权: %v", err)
	}
	if err := st.RememberSessionApproval(ctx, "ses_1", "key-a", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("记录新授权: %v", err)
	}
	// 续期：同一 (会话, 键) 覆盖而不是插第二条。
	if err := st.RememberSessionApproval(ctx, "ses_1", "key-a", now, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("续期: %v", err)
	}
	if ok, _ := st.SessionApproval(ctx, "ses_1", "key-a", now.Add(90*time.Minute)); !ok {
		t.Fatal("续期后应继续命中")
	}

	// 显式清理也应可用，且重复调用无害。
	n, err := st.PruneSessionApprovals(ctx, now)
	if err != nil {
		t.Fatalf("PruneSessionApprovals: %v", err)
	}
	if n != 0 {
		t.Fatalf("不该还有可清理的记录，实际删了 %d 条", n)
	}
	n, err = st.PruneSessionApprovals(ctx, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("PruneSessionApprovals: %v", err)
	}
	if n != 1 {
		t.Fatalf("应该清掉 1 条过期授权，实际 %d", n)
	}
}

func TestSessionApprovalRejectsEmptyArgs(t *testing.T) {
	ctx := context.Background()
	st := openApprovalStore(t, filepath.Join(t.TempDir(), "jiaoag.db"))
	now := time.Now()

	if _, err := st.SessionApproval(ctx, "", "key", now); err == nil {
		t.Fatal("空会话应报错")
	}
	if _, err := st.SessionApproval(ctx, "ses_1", "", now); err == nil {
		t.Fatal("空键应报错")
	}
	if err := st.RememberSessionApproval(ctx, "", "key", now, now.Add(time.Hour)); err == nil {
		t.Fatal("空会话应报错")
	}
}
