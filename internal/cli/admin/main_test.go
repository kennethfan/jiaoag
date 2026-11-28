package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/cli/admin"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

// seedOldSession 造一个「2020 年之后再没动过」的会话，并写一份最小配置。
func seedOldSession(t *testing.T) (cfgPath string, sessionID domain.SessionID) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jiaoag.db")

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	sessionID = domain.SessionID("ses_old")
	sess := domain.Session{
		ID:        sessionID,
		Workspace: dir,
		Title:     "旧会话",
		CreatedAt: domain.Now(),
		UpdatedAt: domain.Now(),
	}
	ev, err := domain.NewEvent(domain.EventSessionCreated, sessionID, "", domain.SessionCreatedPayload{
		Workspace: dir,
		Title:     "旧会话",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(ctx, sess, []domain.Event{ev}); err != nil {
		t.Fatalf("建会话: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开原始连接: %v", err)
	}
	if _, err := raw.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		"2020-01-01T00:00:00Z", string(sessionID)); err != nil {
		t.Fatalf("改写 updated_at: %v", err)
	}
	raw.Close()

	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := "storage:\n  db_path: " + dbPath + "\n  output_dir: " + filepath.Join(dir, "out") + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, sessionID
}

func sessionExists(t *testing.T, cfgPath string, id domain.SessionID) bool {
	t.Helper()
	// 从配置里取库路径，避免测试自己再拼一遍。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var dbPath string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "db_path:"); ok {
			dbPath = strings.TrimSpace(v)
		}
	}
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer db.Close()
	if _, err := db.GetSession(context.Background(), id); err != nil {
		return false
	}
	return true
}

func TestPruneDryRunListsWithoutDeleting(t *testing.T) {
	cfgPath, id := seedOldSession(t)
	var out, errOut bytes.Buffer
	code := admin.Run([]string{"prune", "-config", cfgPath, "-older-than", "90d", "-dry-run"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("退出码 %d，stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), string(id)) {
		t.Errorf("dry-run 应列出会话 %s，实际输出：\n%s", id, out.String())
	}
	if !strings.Contains(out.String(), "未删除任何数据") {
		t.Errorf("dry-run 应说明未删除：\n%s", out.String())
	}
	if !sessionExists(t, cfgPath, id) {
		t.Error("dry-run 不应删除会话")
	}
}

func TestPruneRequiresConfirmation(t *testing.T) {
	cfgPath, id := seedOldSession(t)
	var out, errOut bytes.Buffer
	code := admin.Run([]string{"prune", "-config", cfgPath, "-older-than", "90d"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("不带 -yes 应返回 1，实际 %d", code)
	}
	if !strings.Contains(out.String(), "确认请加 -yes") {
		t.Errorf("应提示确认方式：\n%s", out.String())
	}
	if !sessionExists(t, cfgPath, id) {
		t.Error("未确认时不应删除")
	}
}

func TestPruneDeletesWithConfirmation(t *testing.T) {
	cfgPath, id := seedOldSession(t)
	var out, errOut bytes.Buffer
	code := admin.Run([]string{"prune", "-config", cfgPath, "-older-than", "90d", "-yes"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("退出码 %d，stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "已删除 1 个会话") {
		t.Errorf("应报告删除数量：\n%s", out.String())
	}
	if sessionExists(t, cfgPath, id) {
		t.Error("会话应已被删除")
	}
}

func TestPruneRejectsBadRetention(t *testing.T) {
	cfgPath, _ := seedOldSession(t)
	var out, errOut bytes.Buffer
	for _, bad := range []string{"", "0", "-3d", "abc"} {
		code := admin.Run([]string{"prune", "-config", cfgPath, "-older-than", bad, "-yes"}, &out, &errOut)
		if code != 2 {
			t.Errorf("-older-than %q 应返回 2，实际 %d", bad, code)
		}
	}
}

func TestUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := admin.Run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("未知子命令应返回 2，实际 %d", code)
	}
	if !strings.Contains(errOut.String(), "未知子命令") {
		t.Errorf("stderr 应给出提示：%s", errOut.String())
	}
}
