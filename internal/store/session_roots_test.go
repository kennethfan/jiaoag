package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

func TestCreateSessionPersistsRoots(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer s.Close()

	want := []domain.SessionRoot{{Alias: "lib", Path: "/tmp/lib"}, {Alias: "docs", Path: "/tmp/docs"}}
	if _, err := s.CreateSession(ctx, domain.Session{
		ID:        "ses_1",
		Workspace: t.TempDir(),
		Roots:     want,
	}, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession(ctx, "ses_1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(got.Roots) != 2 || got.Roots[0] != want[0] || got.Roots[1] != want[1] {
		t.Fatalf("附加根读回不一致：%+v", got.Roots)
	}

	// 没有附加根的会话读回空列表，而不是报错。
	if _, err := s.CreateSession(ctx, domain.Session{ID: "ses_2", Workspace: t.TempDir()}, nil); err != nil {
		t.Fatalf("CreateSession 无附加根: %v", err)
	}
	plain, err := s.GetSession(ctx, "ses_2")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(plain.Roots) != 0 {
		t.Fatalf("无附加根读回非空：%+v", plain.Roots)
	}
}
