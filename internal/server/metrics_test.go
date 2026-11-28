package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/store"
)

// newMetricsServer 起一个带应用的 server，并把库句柄交给测试去种数据。
func newMetricsServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0", Token: "test-token"},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, log, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	db, err := store.Open(context.Background(), cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	application := app.New(db, event.NewHub())
	srv.WithApp(application)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = application.Close()
	})
	return ts, db
}

func getMetrics(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestMetricsRequiresToken(t *testing.T) {
	ts, _ := newMetricsServer(t)
	code, _ := getMetrics(t, ts.URL, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", code)
	}
	code, _ = getMetrics(t, ts.URL, "wrong-token")
	if code != http.StatusUnauthorized {
		t.Fatalf("错令牌应 401，得到 %d", code)
	}
}

func TestMetricsReportsProcessAndStore(t *testing.T) {
	ts, db := newMetricsServer(t)
	ctx := context.Background()
	ws := t.TempDir()

	created, err := domain.NewEvent(domain.EventSessionCreated, "ses_metrics", "", domain.SessionCreatedPayload{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(ctx, domain.Session{
		ID: "ses_metrics", Workspace: ws, CreatedAt: domain.Now(), UpdatedAt: domain.Now(),
	}, []domain.Event{created}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	queued, err := domain.NewEvent(domain.EventTaskQueued, "ses_metrics", "tsk_metrics", domain.TaskQueuedPayload{Prompt: "指标测试"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, domain.Task{
		ID: "tsk_metrics", SessionID: "ses_metrics", Status: domain.TaskQueued, Prompt: "指标测试",
	}, []domain.Event{queued}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	code, body := getMetrics(t, ts.URL, "test-token")
	if code != http.StatusOK {
		t.Fatalf("带令牌应 200，得到 %d：%s", code, body)
	}
	for _, want := range []string{
		"# TYPE jiaoag_info gauge",
		`jiaoag_info{version="test",protocol="1"} 1`,
		"jiaoag_uptime_seconds ",
		"jiaoag_goroutines ",
		"jiaoag_connections 0",
		"jiaoag_subscriptions 0",
		"jiaoag_memory_alloc_bytes ",
		"jiaoag_gc_cycles ",
		"jiaoag_sessions 1",
		`jiaoag_tasks{status="queued"} 1`,
		"jiaoag_events 2",
		"jiaoag_audit_rows ",
		`jiaoag_background_procs{status="running"} 0`,
		`jiaoag_background_procs{status="orphaned"} 0`,
		"jiaoag_metrics_error 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("指标里缺少 %q：\n%s", want, body)
		}
	}
}
