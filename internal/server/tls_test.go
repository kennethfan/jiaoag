package server

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/tlstest"
)

// 配了证书对就必须走 TLS：healthz 与 /ws 共用同一个监听，客户端也只能用 wss 连。
func TestTLSServesHealthAndWebSocket(t *testing.T) {
	cert, key := tlstest.Pair(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := &config.Config{
		Server: config.Server{
			Listen:  addr,
			Token:   testToken,
			TLSCert: cert,
			TLSKey:  key,
		},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetTLS(cert, key)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	client := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	waitSecureHealthy(t, client, addr)

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, "wss://"+addr+"/ws", &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testToken}},
	})
	if err != nil {
		t.Fatalf("wss 连接失败：%v", err)
	}
	defer conn.CloseNow()

	var env protocol.Envelope
	if err := wsjson.Read(dialCtx, conn, &env); err != nil {
		t.Fatalf("读取 hello 失败：%v", err)
	}
	if env.Type != protocol.TypeHello {
		t.Fatalf("期待 hello，收到 %s", env.Type)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("关闭时应正常返回：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消 ctx 后服务端未退出")
	}
}

// 明文客户端连不上 TLS 端口，确认不是「悄悄同时开两个端口」。
func TestTLSPortRejectsPlainHTTP(t *testing.T) {
	cert, key := tlstest.Pair(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := &config.Config{
		Server:  config.Server{Listen: addr, Token: testToken, TLSCert: cert, TLSKey: key},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetTLS(cert, key)
	// 明文探测会触发标准库的 TLS 握手告警，属于本用例的预期输入，静音。
	srv.httpSrv.ErrorLog = log.New(io.Discard, "", 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	secure := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	waitSecureHealthy(t, secure, addr)

	plain := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := plain.Get("http://" + addr + "/healthz")
	if err == nil {
		_ = resp.Body.Close()
		// Go 的 TLS 监听会直接回 400「client sent an HTTP request to an HTTPS
		// server」；只要不是 200，就说明明文确实没被当成正常请求处理。
		if resp.StatusCode == http.StatusOK {
			t.Fatal("明文请求不该拿到 200")
		}
	}
}

func waitSecureHealthy(t *testing.T, client *http.Client, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("TLS 服务端未就绪：%s", addr)
}
