package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// newLedgerServer 起一个接了真实 SQLite 的服务端，用于验证接缝 A。
func newLedgerServer(t *testing.T) (*httptest.Server, *app.App) {
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
	application := app.New(db, event.NewHub())
	srv.WithApp(application)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = application.Close()
	})
	return ts, application
}

func write(t *testing.T, conn *websocket.Conn, env protocol.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, env); err != nil {
		t.Fatalf("发送 %s: %v", env.Type, err)
	}
}

func TestSessionAndTaskOverWS(t *testing.T) {
	ts, _ := newLedgerServer(t)
	conn := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, conn) // hello
	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, conn, ch)

	create, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: t.TempDir(),
		Title:     "WS 会话",
	})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, conn, create)

	created := readEnvelope(t, conn)
	if created.Type != protocol.TypeSessionCreated {
		t.Fatalf("类型 = %s，期望 session.created", created.Type)
	}
	var sp protocol.SessionPayload
	if err := created.Decode(&sp); err != nil {
		t.Fatalf("解码会话: %v", err)
	}

	submit, err := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{
		SessionID: sp.ID,
		Text:      "给 X 加测试",
	})
	if err != nil {
		t.Fatalf("构造 task.submit: %v", err)
	}
	write(t, conn, submit)

	queued := readEnvelope(t, conn)
	if queued.Type != protocol.TypeTaskQueued {
		t.Fatalf("类型 = %s，期望 task.queued", queued.Type)
	}
	var tp protocol.TaskPayload
	if err := queued.Decode(&tp); err != nil {
		t.Fatalf("解码任务: %v", err)
	}
	if tp.Status != "queued" {
		t.Fatalf("状态 = %s，期望 queued", tp.Status)
	}

	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: sp.ID,
		FromSeq:   0,
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)

	for want := int64(1); want <= 2; want++ {
		env := readEnvelope(t, conn)
		if env.Type != protocol.TypeEvent {
			t.Fatalf("类型 = %s，期望 event", env.Type)
		}
		if env.Seq != want {
			t.Fatalf("seq = %d，期望 %d", env.Seq, want)
		}
		var payload protocol.StreamEventPayload
		if err := env.Decode(&payload); err != nil {
			t.Fatalf("解码事件: %v", err)
		}
		if payload.Kind == "" {
			t.Fatal("事件缺少 kind")
		}
	}
}

func TestReplayAfterReconnect(t *testing.T) {
	ts, application := newLedgerServer(t)
	ctx := context.Background()

	conn := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, conn)
	ch, _ := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion: "test-client", ProtocolVersion: protocol.Version,
	})
	write(t, conn, ch)

	create, _ := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{Workspace: t.TempDir()})
	write(t, conn, create)
	created := readEnvelope(t, conn)
	var sp protocol.SessionPayload
	if err := created.Decode(&sp); err != nil {
		t.Fatalf("解码会话: %v", err)
	}
	submit, _ := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{SessionID: sp.ID, Text: "第一件"})
	write(t, conn, submit)
	_ = readEnvelope(t, conn) // task.queued

	// 断线期间继续产生事件。
	conn.CloseNow()
	if _, err := application.SubmitTask(ctx, domain.SessionID(sp.ID), "第二件"); err != nil {
		t.Fatalf("断线期间提交任务: %v", err)
	}

	conn2 := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, conn2)
	ch2, _ := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion: "test-client", ProtocolVersion: protocol.Version,
	})
	write(t, conn2, ch2)

	sub, _ := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: sp.ID,
		FromSeq:   2,
	})
	write(t, conn2, sub)

	env := readEnvelope(t, conn2)
	if env.Type != protocol.TypeEvent || env.Seq != 3 {
		t.Fatalf("回放应只补第 3 条，得到 %s#%d", env.Type, env.Seq)
	}
	var payload protocol.StreamEventPayload
	if err := env.Decode(&payload); err != nil {
		t.Fatalf("解码事件: %v", err)
	}
	if payload.Kind != "task.queued" {
		t.Fatalf("事件类型 = %s，期望 task.queued", payload.Kind)
	}
}
