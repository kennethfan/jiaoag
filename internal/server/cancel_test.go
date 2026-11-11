package server

import (
	"context"
	"testing"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func handshake(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = readEnvelope(t, conn) // hello
	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, conn, ch)
}

func TestTaskCancelOverWS(t *testing.T) {
	ts, application := newLedgerServer(t)
	ctx := context.Background()
	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := application.SubmitTask(ctx, sess.ID, "排队中")
	if err != nil {
		t.Fatal(err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	cancel, err := protocol.New(protocol.TypeTaskCancel, protocol.TaskCancelPayload{
		SessionID: string(sess.ID),
		TaskID:    string(task.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, conn, cancel)

	reply := readEnvelope(t, conn)
	if reply.Type != protocol.TypeTaskCanceled {
		t.Fatalf("类型 = %s，期望 task.canceled", reply.Type)
	}

	done, err := application.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.TaskCanceled {
		t.Fatalf("任务状态 = %s，期望 canceled", done.Status)
	}
}

func TestTaskCancelRejectsUnknownTask(t *testing.T) {
	ts, _ := newLedgerServer(t)
	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	unknown, err := protocol.New(protocol.TypeTaskCancel, protocol.TaskCancelPayload{
		SessionID: "ses_unknown",
		TaskID:    "tsk_unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, conn, unknown)

	reply := readEnvelope(t, conn)
	if reply.Type != protocol.TypeError {
		t.Fatalf("类型 = %s，期望 error", reply.Type)
	}
	var p protocol.ErrorPayload
	if err := reply.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s，期望 %s", p.Code, protocol.CodeBadRequest)
	}
}

func TestDeltaFrameIsTemporary(t *testing.T) {
	ts, application := newLedgerServer(t)
	ctx := context.Background()
	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "m", "")
	if err != nil {
		t.Fatal(err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)
	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
		FromSeq:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, conn, sub)

	// 回放 session.created。
	first := readEnvelope(t, conn)
	if first.Type != protocol.TypeEvent || first.Seq == 0 {
		t.Fatalf("第一帧 = %s seq=%d，期望带 seq 的 event", first.Type, first.Seq)
	}

	delta, err := domain.NewEvent(domain.EventAssistantDelta, sess.ID, "tsk_1", domain.AssistantDeltaPayload{Text: "流式"})
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Delta(ctx, delta); err != nil {
		t.Fatalf("Delta: %v", err)
	}

	got := readEnvelope(t, conn)
	if got.Type != protocol.TypeEvent {
		t.Fatalf("类型 = %s", got.Type)
	}
	if got.Seq != 0 {
		t.Fatalf("临时帧不应带 seq，得到 %d", got.Seq)
	}
	var payload protocol.StreamEventPayload
	if err := got.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != string(domain.EventAssistantDelta) {
		t.Fatalf("kind = %s", payload.Kind)
	}

	// 重连回放不含临时帧：库里没有这条事件。
	replayed, err := application.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range replayed {
		if ev.Type == domain.EventAssistantDelta {
			t.Fatal("临时帧不应进入事件日志")
		}
	}
}
