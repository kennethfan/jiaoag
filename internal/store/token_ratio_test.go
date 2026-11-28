package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/store"
)

func openRatioStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTokenRatioLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openRatioStore(t)

	ratio, err := db.TokenRatio(ctx, "agnes/agnes-3.0-flash")
	if err != nil {
		t.Fatal(err)
	}
	if ratio != 0 {
		t.Fatalf("没有记录时应返回 0，得到 %v", ratio)
	}

	if err := db.SaveTokenRatio(ctx, "agnes/agnes-3.0-flash", 1.8); err != nil {
		t.Fatal(err)
	}
	ratio, err = db.TokenRatio(ctx, "agnes/agnes-3.0-flash")
	if err != nil {
		t.Fatal(err)
	}
	if ratio != 1.8 {
		t.Fatalf("读回的系数应为 1.8，得到 %v", ratio)
	}
	if samples, err := db.TokenRatioSamples(ctx, "agnes/agnes-3.0-flash"); err != nil || samples != 1 {
		t.Fatalf("样本数应为 1，得到 %d（err=%v）", samples, err)
	}

	// 再写一次是覆盖系数、累加样本数，而不是插出第二行。
	if err := db.SaveTokenRatio(ctx, "agnes/agnes-3.0-flash", 2.2); err != nil {
		t.Fatal(err)
	}
	ratio, _ = db.TokenRatio(ctx, "agnes/agnes-3.0-flash")
	if ratio != 2.2 {
		t.Fatalf("后写者应覆盖，得到 %v", ratio)
	}
	if samples, _ := db.TokenRatioSamples(ctx, "agnes/agnes-3.0-flash"); samples != 2 {
		t.Fatalf("样本数应累加到 2，得到 %d", samples)
	}

	// 键之间互不影响：换模型不会读到上一个模型的系数。
	if ratio, _ := db.TokenRatio(ctx, "agnes/agnes-2.5-flash"); ratio != 0 {
		t.Fatalf("另一个键不该命中，得到 %v", ratio)
	}
}

func TestTokenRatioRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	db := openRatioStore(t)

	if _, err := db.TokenRatio(ctx, ""); err == nil {
		t.Fatal("空键应报错")
	}
	if err := db.SaveTokenRatio(ctx, "", 1.2); err == nil {
		t.Fatal("空键应报错")
	}
	if err := db.SaveTokenRatio(ctx, "a/b", 0); err == nil {
		t.Fatal("非正系数应报错")
	}
	if err := db.SaveTokenRatio(ctx, "a/b", -1); err == nil {
		t.Fatal("负系数应报错")
	}
}
