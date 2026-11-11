package server

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// TestBeginStreamCancelsPreviousOnSameSession 锁住订阅登记表的核心不变量：
// 同一会话重复订阅取消旧的、不同会话互不影响、迟到的旧句柄不会抹掉新订阅。
func TestBeginStreamCancelsPreviousOnSameSession(t *testing.T) {
	c := &clientConn{}

	oldCtx, cancelOld := context.WithCancel(context.Background())
	oldDone := make(chan struct{})
	go func() {
		<-oldCtx.Done()
		close(oldDone)
	}()
	first := c.beginStream("ses_1", cancelOld)

	otherCtx, cancelOther := context.WithCancel(context.Background())
	otherDone := make(chan struct{})
	go func() {
		<-otherCtx.Done()
		close(otherDone)
	}()
	c.beginStream("ses_2", cancelOther)

	select {
	case <-otherDone:
		t.Fatal("不同会话的订阅不应被取消")
	default:
	}

	_, cancelNew := context.WithCancel(context.Background())
	second := c.beginStream("ses_1", cancelNew)

	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("重复订阅应取消旧协程")
	}

	c.endStream("ses_1", first)
	c.subsMu.Lock()
	got := c.subs["ses_1"]
	c.subsMu.Unlock()
	if got != second {
		t.Fatal("迟到的旧句柄注销把新订阅抹掉了")
	}

	c.endStream("ses_1", second)
	c.subsMu.Lock()
	_, ok := c.subs["ses_1"]
	c.subsMu.Unlock()
	if ok {
		t.Fatal("注销后仍残留订阅")
	}
}

// TestResubscribeOnSameConnDoesNotDuplicate 端到端验证：同一连接二次订阅后，
// 新产生的事件只推送一次。
func TestResubscribeOnSameConnDoesNotDuplicate(t *testing.T) {
	ts, application := newLedgerServer(t)
	conn := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, conn) // hello

	hello, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, conn, hello)

	create, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, conn, create)
	created := readEnvelope(t, conn)
	var sp protocol.SessionPayload
	if err := created.Decode(&sp); err != nil {
		t.Fatalf("解码会话: %v", err)
	}

	submit, err := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{SessionID: sp.ID, Text: "第一件"})
	if err != nil {
		t.Fatalf("构造 task.submit: %v", err)
	}
	write(t, conn, submit)
	_ = readEnvelope(t, conn) // task.queued

	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{SessionID: sp.ID, FromSeq: 0})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)
	for want := int64(1); want <= 2; want++ {
		env := readEnvelope(t, conn)
		if env.Type != protocol.TypeEvent || env.Seq != want {
			t.Fatalf("回放得到 %s#%d，期望 event#%d", env.Type, env.Seq, want)
		}
	}

	// 第二次订阅同一会话；用 ping/pong 做屏障，确保重订阅已被处理。
	sub2, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{SessionID: sp.ID, FromSeq: 2})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub2)
	ping, err := protocol.New(protocol.TypePing, nil)
	if err != nil {
		t.Fatalf("构造 ping: %v", err)
	}
	write(t, conn, ping)
	if env := readEnvelope(t, conn); env.Type != protocol.TypePong {
		t.Fatalf("屏障失败，收到 %s", env.Type)
	}

	if _, err := application.SubmitTask(context.Background(), domain.SessionID(sp.ID), "第二件"); err != nil {
		t.Fatalf("提交任务: %v", err)
	}

	env := readEnvelope(t, conn)
	if env.Type != protocol.TypeEvent || env.Seq != 3 {
		t.Fatalf("得到 %s#%d，期望 event#3", env.Type, env.Seq)
	}
	if extra, ok := readEnvelopeWithin(conn, 300*time.Millisecond); ok {
		t.Fatalf("重复订阅导致重复推送: %s#%d", extra.Type, extra.Seq)
	}
}

// readEnvelopeWithin 在 d 内读一帧；超时返回 ok=false，用于断言「没有多余帧」。
func readEnvelopeWithin(conn *websocket.Conn, d time.Duration) (protocol.Envelope, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var env protocol.Envelope
	if err := wsjson.Read(ctx, conn, &env); err != nil {
		return protocol.Envelope{}, false
	}
	return env, true
}
