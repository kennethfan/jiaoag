package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

// 库不存在时不能顺手建一个：只读命令失败要比「安静地写了个空库」安全。
func TestOpenReadOnlyRequiresExistingFile(t *testing.T) {
	_, err := store.OpenReadOnly(context.Background(), filepath.Join(t.TempDir(), "missing.db"))
	if err == nil || !strings.Contains(err.Error(), "还不存在") {
		t.Fatalf("期望提示库不存在，得到 %v", err)
	}
}

// 空文件不是 jiaoag 的库：给出「先启动一次」，而不是 SQL 的 no such table。
func TestOpenReadOnlyRejectsUninitialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.OpenReadOnly(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "还没有初始化") {
		t.Fatalf("期望提示未初始化，得到 %v", err)
	}
}

func TestOpenReadOnlyReadsExistingData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.CreateSession(ctx, domain.Session{ID: "ses_ro", Workspace: t.TempDir(), Title: "只读"}, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	sessions, err := ro.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "ses_ro" || sessions[0].Title != "只读" {
		t.Fatalf("读到的会话不对: %+v", sessions)
	}
}

// 结构落后时拒绝读取：旧列/旧表会让查询在半路报错，不如提前说清楚。
func TestOpenReadOnlyRejectsOldSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 用普通连接删掉高版本记录，模拟「库是旧程序建的」。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version > 5`); err != nil {
		t.Fatalf("降级: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭写连接: %v", err)
	}

	_, err = store.OpenReadOnly(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "结构版本") {
		t.Fatalf("期望提示结构版本落后，得到 %v", err)
	}
}
