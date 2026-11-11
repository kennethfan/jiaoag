package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

const testToken = "test-token"

func newTestServer(t *testing.T) *httptest.Server {
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
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

func dial(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var header http.Header
	if token != "" {
		header = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func readEnvelope(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var env protocol.Envelope
	if err := wsjson.Read(ctx, conn, &env); err != nil {
		t.Fatalf("读取帧失败: %v", err)
	}
	return env
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("响应体缺少 status=ok: %s", body)
	}
	if !strings.Contains(string(body), `"protocol_version":1`) {
		t.Fatalf("响应体缺少协议版本: %s", body)
	}
}

func TestWSRejectsMissingToken(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(ts), nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("无 token 竟然握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401，得到 %+v", resp)
	}
}

func TestWSRejectsWrongToken(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer wrong"}},
	})
	if err == nil {
		conn.CloseNow()
		t.Fatal("错误 token 竟然握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("期望 401，得到 %+v", resp)
	}
}

func TestHandshakeThenPingPong(t *testing.T) {
	ts := newTestServer(t)
	conn := dial(t, wsURL(ts), testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hello := readEnvelope(t, conn)
	if hello.Type != protocol.TypeHello {
		t.Fatalf("第一帧类型 = %s，期望 hello", hello.Type)
	}
	var payload protocol.HelloPayload
	if err := hello.Decode(&payload); err != nil {
		t.Fatalf("解码 hello: %v", err)
	}
	if payload.ProtocolVersion != protocol.Version {
		t.Fatalf("协议版本 = %d，期望 %d", payload.ProtocolVersion, protocol.Version)
	}

	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	if err := wsjson.Write(ctx, conn, ch); err != nil {
		t.Fatalf("发送 client.hello: %v", err)
	}

	ping, err := protocol.New(protocol.TypePing, nil)
	if err != nil {
		t.Fatalf("构造 ping: %v", err)
	}
	if err := wsjson.Write(ctx, conn, ping); err != nil {
		t.Fatalf("发送 ping: %v", err)
	}

	pong := readEnvelope(t, conn)
	if pong.Type != protocol.TypePong {
		t.Fatalf("回帧类型 = %s，期望 pong", pong.Type)
	}
}

func TestProtocolMismatchIsReported(t *testing.T) {
	ts := newTestServer(t)
	conn := dial(t, wsURL(ts), testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = readEnvelope(t, conn) // hello

	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "future-client",
		ProtocolVersion: protocol.Version + 99,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	if err := wsjson.Write(ctx, conn, ch); err != nil {
		t.Fatalf("发送 client.hello: %v", err)
	}

	env := readEnvelope(t, conn)
	if env.Type != protocol.TypeError {
		t.Fatalf("类型 = %s，期望 error", env.Type)
	}
	var p protocol.ErrorPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码 error: %v", err)
	}
	if p.Code != protocol.CodeProtocolMismatch {
		t.Fatalf("错误码 = %s，期望 %s", p.Code, protocol.CodeProtocolMismatch)
	}
}
