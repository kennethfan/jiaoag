package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/store"
)

func openTokenStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestServerTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTokenStore(t)

	v0, err := db.ServerTokenVersion(ctx)
	if err != nil {
		t.Fatalf("读取初始版本: %v", err)
	}

	if err := db.CreateServerToken(ctx, "ops", "hash-1", false); err != nil {
		t.Fatalf("创建令牌: %v", err)
	}
	if err := db.CreateServerToken(ctx, "viewer", "hash-2", true); err != nil {
		t.Fatalf("创建只读令牌: %v", err)
	}
	if err := db.CreateServerToken(ctx, "ops", "hash-x", false); !errors.Is(err, store.ErrTokenExists) {
		t.Fatalf("重名应报 ErrTokenExists，实际 %v", err)
	}

	rows, err := db.ServerTokens(ctx)
	if err != nil {
		t.Fatalf("列出令牌: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条令牌，实际 %d", len(rows))
	}
	if rows[0].Name != "ops" || rows[0].ReadOnly || rows[0].Hash != "hash-1" || rows[0].Revoked {
		t.Fatalf("ops 记录不对: %+v", rows[0])
	}
	if !rows[1].ReadOnly {
		t.Fatalf("viewer 应是只读: %+v", rows[1])
	}

	// 每次写入都要推进版本号，否则热更新看不到变化。
	v1, err := db.ServerTokenVersion(ctx)
	if err != nil {
		t.Fatalf("读取版本: %v", err)
	}
	if v1 != v0+2 {
		t.Fatalf("创建两条后版本应 +2（%d → %d）", v0, v1)
	}

	if err := db.RotateServerToken(ctx, "ops", "hash-3"); err != nil {
		t.Fatalf("轮换令牌: %v", err)
	}
	rows, _ = db.ServerTokens(ctx)
	if rows[0].Hash != "hash-3" || rows[0].RotatedAt.IsZero() {
		t.Fatalf("轮换后哈希/时间不对: %+v", rows[0])
	}
	if err := db.RotateServerToken(ctx, "nope", "hash"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("轮换不存在的令牌应报 ErrTokenNotFound，实际 %v", err)
	}

	if err := db.RevokeServerToken(ctx, "viewer"); err != nil {
		t.Fatalf("吊销令牌: %v", err)
	}
	rows, _ = db.ServerTokens(ctx)
	if !rows[1].Revoked {
		t.Fatalf("viewer 应标记为已吊销: %+v", rows[1])
	}
	if err := db.RevokeServerToken(ctx, "viewer"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("重复吊销应报 ErrTokenNotFound，实际 %v", err)
	}
	if err := db.RotateServerToken(ctx, "viewer", "hash-4"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("吊销后不能再轮换，实际 %v", err)
	}

	v2, _ := db.ServerTokenVersion(ctx)
	if v2 <= v1 {
		t.Fatalf("轮换与吊销也应推进版本号（%d → %d）", v1, v2)
	}
}

func TestServerTokenRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTokenStore(t)
	if err := db.CreateServerToken(ctx, "", "hash", false); err == nil {
		t.Fatal("空名字应被拒绝")
	}
	if err := db.CreateServerToken(ctx, "a", "", false); err == nil {
		t.Fatal("空哈希应被拒绝")
	}
	if err := db.RevokeServerToken(ctx, ""); err == nil {
		t.Fatal("空名字吊销应被拒绝")
	}
}
