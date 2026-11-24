package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// newObserveServer 起一台带真实存储的服务端，并预置一个会话、一个任务与
// 两条审计（一条关联工具调用，一条没有）。
func newObserveServer(t *testing.T) (*httptest.Server, domain.Session, domain.Task) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0", Token: testToken},
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
	t.Cleanup(func() { _ = db.Close() })

	sess := domain.Session{ID: "ses_obs", Workspace: t.TempDir(), Title: "观测"}
	created, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{
		Workspace: sess.Workspace, Title: sess.Title,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.CreateSession(context.Background(), sess, []domain.Event{created}); err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	task := domain.Task{ID: "tsk_obs", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "看一下"}
	queued, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, task.ID, domain.TaskQueuedPayload{Prompt: task.Prompt})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := db.CreateTask(context.Background(), task, []domain.Event{queued}); err != nil {
		t.Fatalf("创建任务: %v", err)
	}

	call := domain.ToolCall{ID: "tc_obs", TaskID: task.ID, SessionID: sess.ID, Name: "run_command", Status: domain.ToolSucceeded}
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
	return ts, sess, task
}

// observeConn 完成握手，返回可用于发命令的连接。
func observeConn(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	conn := dial(t, wsURL(ts), testToken)
	hello := readEnvelope(t, conn)
	if hello.Type != protocol.TypeHello {
		t.Fatalf("期待 hello，收到 %s", hello.Type)
	}
	write(t, conn, envelope(t, protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion: "test", ProtocolVersion: protocol.Version,
	}))
	return conn
}

func envelope(t *testing.T, typ string, payload any) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(typ, payload)
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	return env
}

func TestAuditTailBootstrapReturnsHead(t *testing.T) {
	ts, _, _ := newObserveServer(t)
	conn := observeConn(t, ts)

	write(t, conn, envelope(t, protocol.TypeAuditTail, protocol.AuditTailPayload{AfterID: -1}))
	env := readEnvelope(t, conn)
	if env.Type != protocol.TypeAuditTailResult {
		t.Fatalf("期待 audit.tail.result，收到 %s", env.Type)
	}
	var p protocol.AuditTailResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if len(p.Records) != 0 {
		t.Fatalf("取水位时不该带记录，得到 %d", len(p.Records))
	}
	if p.NextID != 2 {
		t.Fatalf("水位应为最后一条审计 id=2，得到 %d", p.NextID)
	}
}

func TestAuditTailExportsRecordsWithAttribution(t *testing.T) {
	ts, sess, task := newObserveServer(t)
	conn := observeConn(t, ts)

	write(t, conn, envelope(t, protocol.TypeAuditTail, protocol.AuditTailPayload{AfterID: 0, Limit: 10}))
	env := readEnvelope(t, conn)
	var p protocol.AuditTailResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if len(p.Records) != 2 {
		t.Fatalf("期望 2 条审计，得到 %d", len(p.Records))
	}
	first := p.Records[0]
	if first.Action != "exec.approved" || first.TaskID != string(task.ID) || first.SessionID != string(sess.ID) {
		t.Fatalf("审计归属不对: %+v", first)
	}
	if first.At == 0 {
		t.Fatal("审计时间丢了")
	}
	if len(first.Detail) == 0 {
		t.Fatal("detail 丢了")
	}
	if p.NextID != p.Records[1].ID {
		t.Fatalf("next_id=%d 应等于最后一条 id=%d", p.NextID, p.Records[1].ID)
	}
	// 没有工具调用的那条照样导出，只是没有任务归属。
	if p.Records[1].Action != "context.compact" || p.Records[1].TaskID != "" {
		t.Fatalf("无归属的审计被丢或被错关联: %+v", p.Records[1])
	}
}

func TestEventHeadReportsLastSeq(t *testing.T) {
	ts, sess, _ := newObserveServer(t)
	conn := observeConn(t, ts)

	write(t, conn, envelope(t, protocol.TypeEventHead, protocol.EventHeadPayload{SessionID: string(sess.ID)}))
	env := readEnvelope(t, conn)
	if env.Type != protocol.TypeEventHeadResult {
		t.Fatalf("期待 event.head.result，收到 %s", env.Type)
	}
	var p protocol.EventHeadResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码: %v", err)
	}
	// 会话创建 + 任务入队 + task.status = 3 条。
	if p.Seq != 3 {
		t.Fatalf("最后序号应为 3，得到 %d", p.Seq)
	}
}

func TestEventHeadRequiresSession(t *testing.T) {
	ts, _, _ := newObserveServer(t)
	conn := observeConn(t, ts)

	write(t, conn, envelope(t, protocol.TypeEventHead, protocol.EventHeadPayload{}))
	env := readEnvelope(t, conn)
	if env.Type != protocol.TypeError {
		t.Fatalf("期待 error，收到 %s", env.Type)
	}
	var p protocol.ErrorPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if p.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码应为 bad_request，得到 %s", p.Code)
	}
}
