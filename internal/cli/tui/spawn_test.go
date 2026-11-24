package tui

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/config"
)

func TestTargetAddr(t *testing.T) {
	cases := []struct {
		target   string
		host     string
		port     string
		hostport string
	}{
		{"ws://127.0.0.1:8787/ws", "127.0.0.1", "8787", "127.0.0.1:8787"},
		{"ws://[::1]:9000/ws", "::1", "9000", "[::1]:9000"},
		{"ws://localhost/ws", "localhost", "80", "localhost:80"},
		{"wss://example.com/ws", "example.com", "443", "example.com:443"},
	}
	for _, c := range cases {
		got, err := targetAddr(c.target)
		if err != nil {
			t.Fatalf("targetAddr(%q): %v", c.target, err)
		}
		if got.host != c.host || got.port != c.port || got.hostport != c.hostport {
			t.Fatalf("targetAddr(%q) = %+v，期望 %s/%s/%s", c.target, got, c.host, c.port, c.hostport)
		}
	}

	if _, err := targetAddr("ws:///ws"); err == nil {
		t.Fatal("缺少主机名应报错")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loops := []string{"localhost", "LOCALHOST", "127.0.0.1", "127.1.2.3", "::1"}
	for _, host := range loops {
		if !isLoopbackHost(host) {
			t.Fatalf("%q 应判定为回环", host)
		}
	}
	for _, host := range []string{"example.com", "10.0.0.2", "0.0.0.0"} {
		if isLoopbackHost(host) {
			t.Fatalf("%q 不应判定为回环", host)
		}
	}
}

func TestResolveServerBinaryExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveServerBinary(path)
	if err != nil {
		t.Fatalf("显式路径: %v", err)
	}
	if got != path {
		t.Fatalf("got %q，期望 %q", got, path)
	}
}

// 默认必须回到自己这个二进制：服务端是同一条命令的 serve 子命令。
func TestResolveServerBinaryDefaultsToSelf(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	got, err := resolveServerBinary("")
	if err != nil {
		t.Fatalf("默认解析: %v", err)
	}
	if got != exe {
		t.Fatalf("got %q，期望自己 %q", got, exe)
	}
}

// 启用 TLS 时探活必须走 https，并且不验证书链（自动拉起的服务端可能用自签证书）。
func TestHealthProbeFollowsTLS(t *testing.T) {
	plain := &config.Config{}
	url, client := healthProbe(plain, "127.0.0.1:8787")
	if url != "http://127.0.0.1:8787/healthz" {
		t.Fatalf("明文探活 URL 不对：%s", url)
	}
	if tr, ok := client.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
		t.Fatal("明文探活不该带 TLS 配置")
	}

	tlsCfg := &config.Config{Server: config.Server{TLSCert: "cert.pem", TLSKey: "key.pem"}}
	url, client = healthProbe(tlsCfg, "127.0.0.1:8787")
	if url != "https://127.0.0.1:8787/healthz" {
		t.Fatalf("TLS 探活 URL 不对：%s", url)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS 探活应跳过证书校验")
	}
}
