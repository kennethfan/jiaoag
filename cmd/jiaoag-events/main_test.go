package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/server"
	"github.com/kennethfan/jiaoag/internal/store"
)

const testToken = "test-token"

// newServer 起一台带种子数据的服务端：一个会话、一个任务、两条审计
// （一条关联工具调用，一条没有）。
func newServer(t *testing.T) (url, sessionID string) {
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
	db, err := store.Open(context.Background(), cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sess := domain.Session{ID: "ses_ev", Workspace: t.TempDir(), Title: "导出"}
	created, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
		Workspace: sess.Workspace, Title: sess.Title,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.CreateSession(context.Background(), sess, []domain.Event{created}); err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	task := domain.Task{ID: "tsk_ev", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "导出"}
	queued, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, task.ID, domain.TaskQueuedPayload{Prompt: task.Prompt})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.CreateTask(context.Background(), task, []domain.Event{queued}); err != nil {
		t.Fatalf("创建任务: %v", err)
	}

	call := domain.ToolCall{ID: "tc_ev", TaskID: task.ID, SessionID: sess.ID, Name: "run_command", Status: domain.ToolSucceeded}
	running, err := domain.NewEvent(domain.EventTaskStatus, sess.ID, task.ID, domain.TaskStatusPayload{
		From: domain.TaskQueued, To: domain.TaskRunning,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.Apply(context.Background(), domain.Transition{
		TaskID:   task.ID,
		To:       domain.TaskRunning,
		ToolCall: &call,
		Audit: []domain.AuditRecord{
			{Actor: "user", Action: "exec.approved", ToolCallID: call.ID, Detail: []byte(`{"argv":["go","test"]}`)},
			{Actor: "system", Action: "context.compact", Detail: []byte(`{"before_chars":4000}`)},
		},
		Events: []domain.Event{running},
	}); err != nil {
		t.Fatalf("写入审计: %v", err)
	}

	srv.WithApp(app.New(db, event.NewHub()))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws", string(sess.ID)
}

// captureStdout 在 fn 执行期间把 stdout 换成管道并返回捕获内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("关闭管道: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读管道: %v", err)
	}
	return string(data)
}

func TestRunExportsEventsAndAuditsThenExits(t *testing.T) {
	url, sessionID := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var runErr error
	out := captureStdout(t, func() {
		runErr = run(ctx, options{
			url: url, token: testToken, sessionID: sessionID,
			from: 0, auditFrom: 0, format: "json",
			untilNow: true, interval: 100 * time.Millisecond,
		})
	})
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 4 {
		t.Fatalf("期望至少 4 行输出（3 条事件 + 1 条审计），得到 %d:\n%s", len(lines), out)
	}
	for _, want := range []string{
		`"kind":"event"`,
		`"type":"task.status"`,
		`"session_id":"` + sessionID + `"`,
		`"kind":"audit"`,
		`"action":"exec.approved"`,
		`"argv":["go","test"]`,
		`"action":"context.compact"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %s:\n%s", want, out)
		}
	}
}

func TestPrinterTextIsOneLinePerRecord(t *testing.T) {
	p := &printer{format: "text"}
	out := captureStdout(t, func() {
		p.event(protocol.Envelope{TS: 1700000000000, Seq: 7, SessionID: "ses_x"},
			protocol.StreamEventPayload{Kind: "tool.result", TaskID: "tsk_x", Payload: []byte(`{"ok":true}`)})
		p.audit(protocol.AuditRecordPayload{
			ID: 3, At: 1700000000000, Actor: "user", Action: "exec.approved",
			TaskID: "tsk_x", ToolCallID: "tc_x", Detail: []byte(`{"argv":["ls"]}`),
		})
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 行，得到 %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "tool.result") || !strings.Contains(lines[0], "tsk_x") {
		t.Fatalf("事件行不对: %s", lines[0])
	}
	if !strings.Contains(lines[1], "audit:exec.approved") || !strings.Contains(lines[1], "user") {
		t.Fatalf("审计行不对: %s", lines[1])
	}
}
