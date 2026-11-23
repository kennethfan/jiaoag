package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/config"
)

func newIdleServer(t *testing.T, idle time.Duration) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := &config.Config{
		Server:  config.Server{Listen: addr, Token: testToken},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, log, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.SetIdleExit(idle)
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("服务端未就绪：%s", addr)
}

// 没连接时倒计时直接生效：TUI 拉起后自己崩了，不留下孤儿进程。
func TestIdleExitWithoutConnections(t *testing.T) {
	srv := newIdleServer(t, 50*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("空闲退出应正常返回：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有连接时应在空闲超时后退出")
	}
}

// 有连接时不能退出；断开后重新倒计时，并在超时后关闭。
func TestIdleExitWaitsForLastConnection(t *testing.T) {
	const idle = 100 * time.Millisecond
	srv := newIdleServer(t, idle)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(context.Background()) }()

	addr := srv.cfg.Server.Listen
	waitHealthy(t, addr)

	conn := dial(t, "ws://"+addr+"/ws", testToken)
	select {
	case err := <-done:
		t.Fatalf("仍有连接时不应退出：%v", err)
	case <-time.After(3 * idle):
	}

	_ = conn.CloseNow()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("空闲退出应正常返回：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("最后一个连接断开后应在空闲超时后退出")
	}
}
