package sessions

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/server"
	"github.com/kennethfan/jiaoag/internal/store"
)

const testToken = "test-token"

// newBackend 起一台带三条会话的服务端：两个在 dirA，一个在 dirB。
// 返回库句柄是为了直接造「忙会话」——那需要一条 queued 任务。
func newBackend(t *testing.T) (opts options, db *store.Store, dirA, dirB string) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0", Token: testToken},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(cfg, log, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	db, err = store.Open(context.Background(), cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	application := app.New(db, event.NewHub())
	t.Cleanup(func() { application.Close() })
	ts := httptest.NewServer(srv.WithApp(application).Handler())
	t.Cleanup(ts.Close)

	dirA = t.TempDir()
	dirB = t.TempDir()
	for _, s := range []domain.Session{
		{ID: "ses_aaaa1111", Workspace: dirA, Title: "甲"},
		{ID: "ses_aaaa2222", Workspace: dirA, Title: "乙"},
		{ID: "ses_bbbb3333", Workspace: dirB, Title: "别处"},
	} {
		sess := s
		ev, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
			Workspace: sess.Workspace, Title: sess.Title,
		})
		if err != nil {
			t.Fatalf("构造事件: %v", err)
		}
		if _, err := db.CreateSession(context.Background(), sess, []domain.Event{ev}); err != nil {
			t.Fatalf("创建会话: %v", err)
		}
	}
	return options{url: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws", token: testToken}, db, dirA, dirB
}

func connect(t *testing.T, opts options) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, err := dial(ctx, opts)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestListFiltersByWorkspace(t *testing.T) {
	opts, _, dirA, _ := newBackend(t)
	conn := connect(t, opts)

	got, err := list(context.Background(), conn, dirA, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("dirA 应有 2 条，得到 %d", len(got))
	}
	for _, s := range got {
		if s.Workspace != dirA {
			t.Fatalf("混进了别的工作区: %s", s.Workspace)
		}
	}
}

func TestListAllCoversOtherWorkspaces(t *testing.T) {
	opts, _, _, _ := newBackend(t)
	conn := connect(t, opts)

	got, err := list(context.Background(), conn, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("-all 应有 3 条，得到 %d", len(got))
	}
}

func TestResolveIDByPrefix(t *testing.T) {
	opts, _, _, _ := newBackend(t)
	conn := connect(t, opts)
	ctx := context.Background()

	id, err := resolveID(ctx, conn, "ses_aaaa2")
	if err != nil {
		t.Fatalf("前缀补全: %v", err)
	}
	if id != "ses_aaaa2222" {
		t.Fatalf("补全结果 = %q", id)
	}
	if _, err := resolveID(ctx, conn, "ses_aaaa"); err == nil || !strings.Contains(err.Error(), "请多给几位") {
		t.Fatalf("歧义前缀应报错，得到 %v", err)
	}
	if _, err := resolveID(ctx, conn, "ses_nope"); err == nil || !strings.Contains(err.Error(), "没有匹配") {
		t.Fatalf("未知前缀应报错，得到 %v", err)
	}
}

func TestRenameAndRemove(t *testing.T) {
	opts, _, dirA, _ := newBackend(t)
	conn := connect(t, opts)
	ctx := context.Background()

	sess, err := rename(ctx, conn, "ses_aaaa1111", "改过的甲")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if sess.Title != "改过的甲" {
		t.Fatalf("回包标题 = %q", sess.Title)
	}

	if err := remove(ctx, conn, "ses_aaaa2222"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	left, err := list(ctx, conn, dirA, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].ID != "ses_aaaa1111" || left[0].Title != "改过的甲" {
		t.Fatalf("删除后剩余 = %+v", left)
	}
}

func TestRemoveRefusesBusySession(t *testing.T) {
	opts, db, _, _ := newBackend(t)
	conn := connect(t, opts)
	ctx := context.Background()

	task := domain.Task{ID: "tsk_busy", SessionID: "ses_aaaa1111", Status: domain.TaskQueued, Prompt: "跑着"}
	queued, err := domain.NewEvent(domain.EventTaskQueued, task.SessionID, task.ID, domain.TaskQueuedPayload{Prompt: task.Prompt})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.CreateTask(ctx, task, []domain.Event{queued}); err != nil {
		t.Fatalf("创建任务: %v", err)
	}

	if err := remove(ctx, conn, "ses_aaaa1111"); err == nil || !strings.Contains(err.Error(), "未结束") {
		t.Fatalf("忙会话应拒绝删除，得到 %v", err)
	}
}

// TestListDirectFiltersWorkspace 验证只读直连：不经过服务端，按工作区筛选
// 且字段映射正确（时间是毫秒）。
func TestListDirectFiltersWorkspace(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/jiaoag.db"
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	for _, s := range []domain.Session{
		{ID: "ses_direct_a1", Workspace: dirA, Title: "甲"},
		{ID: "ses_direct_a2", Workspace: dirA, Title: "乙"},
		{ID: "ses_direct_b1", Workspace: dirB, Title: "别处"},
	} {
		sess := s
		ev, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
			Workspace: sess.Workspace, Title: sess.Title,
		})
		if err != nil {
			t.Fatalf("构造事件: %v", err)
		}
		if _, err := db.CreateSession(ctx, sess, []domain.Event{ev}); err != nil {
			t.Fatalf("创建会话: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}

	cfg := &config.Config{Storage: config.Storage{DBPath: dbPath}}
	got, err := listDirect(ctx, cfg, "", dirA, false)
	if err != nil {
		t.Fatalf("listDirect: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("dirA 应有 2 条会话，得到 %d 条: %+v", len(got), got)
	}
	for _, s := range got {
		if s.Workspace != dirA {
			t.Fatalf("混进了别的工作区: %+v", s)
		}
		if s.CreatedAt <= 0 || s.UpdatedAt <= 0 {
			t.Fatalf("时间应为毫秒时间戳: %+v", s)
		}
	}

	all, err := listDirect(ctx, cfg, "", "", true)
	if err != nil {
		t.Fatalf("listDirect all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("全部应有 3 条会话，得到 %d 条", len(all))
	}
}
