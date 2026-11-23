package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// newMCPTestServer 起一个接入了 MCP 状态查询的服务端。
//
// status 是宿主种进来的替身：真实 Manager 需要能起进程的 server，
// 这里只验协议映射与项目级 server 的传递。
func newMCPTestServer(t *testing.T, status func(root string, extra []mcppkg.Server) []mcppkg.ServerStatus, projectMode string) (*httptest.Server, *app.App) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0", Token: testToken},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
		Project: config.Project{Mode: projectMode},
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
	if err := application.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return idleProvider{}, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MCPServers:    status,
		ProjectMode:   projectMode,
		MaxRounds:     3,
		ContextBudget: 10000,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = application.Close()
	})
	return ts, application
}

func requestMCPList(t *testing.T, ts *httptest.Server, sessionID string) protocol.Envelope {
	t.Helper()
	c := helloConn(t, ts)
	req, err := protocol.New(protocol.TypeMCPList, protocol.MCPListPayload{SessionID: sessionID})
	if err != nil {
		t.Fatalf("构造 mcp.list: %v", err)
	}
	write(t, c, req)
	return readEnvelope(t, c)
}

func TestMCPListReportsServers(t *testing.T) {
	status := func(string, []mcppkg.Server) []mcppkg.ServerStatus {
		return []mcppkg.ServerStatus{
			{
				Name:      "fs",
				Argv:      []string{"npx", "-y", "server-filesystem", "/tmp/ws"},
				Trust:     true,
				Timeout:   60 * time.Second,
				Source:    "config",
				Connected: true,
				Version:   "1.2.0",
				Tools:     14,
			},
			{Name: "lsp", Argv: []string{"mcp-language-server", "-lsp", "gopls"}, Source: "project", Timeout: 120 * time.Second},
		}
	}
	ts, application := newMCPTestServer(t, status, config.ProjectAsk)
	sess, err := application.CreateSession(context.Background(), t.TempDir(), "", "", "MCP")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	env := requestMCPList(t, ts, string(sess.ID))
	if env.Type != protocol.TypeMCPListResult {
		t.Fatalf("回帧 = %s，期望 mcp.list.result", env.Type)
	}
	var p protocol.MCPListResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if len(p.Servers) != 2 {
		t.Fatalf("server 数 = %d，期望 2：%+v", len(p.Servers), p.Servers)
	}
	fs := p.Servers[0]
	if fs.Name != "fs" || !fs.Connected || fs.Tools != 14 || fs.Version != "1.2.0" {
		t.Fatalf("fs 状态不对：%+v", fs)
	}
	if fs.Command != "npx -y server-filesystem /tmp/ws" {
		t.Fatalf("命令拼接 = %q", fs.Command)
	}
	if fs.TimeoutMS != 60000 {
		t.Fatalf("timeout_ms = %d，期望 60000", fs.TimeoutMS)
	}
	if lsp := p.Servers[1]; lsp.Connected || lsp.Source != "project" || lsp.Tools != 0 {
		t.Fatalf("lsp 状态不对：%+v", lsp)
	}
}

func TestMCPListWithoutServersIsEmpty(t *testing.T) {
	status := func(string, []mcppkg.Server) []mcppkg.ServerStatus { return nil }
	ts, application := newMCPTestServer(t, status, config.ProjectAsk)
	sess, err := application.CreateSession(context.Background(), t.TempDir(), "", "", "MCP")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	env := requestMCPList(t, ts, string(sess.ID))
	if env.Type != protocol.TypeMCPListResult {
		t.Fatalf("回帧 = %s", env.Type)
	}
	var p protocol.MCPListResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if len(p.Servers) != 0 {
		t.Fatalf("没配 MCP 时不该有 server：%+v", p.Servers)
	}
}

func TestMCPListSeesProjectServers(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".jiaoag")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "servers:\n  - name: repo\n    command: echo\n    args: [\"hi\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "mcp.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var seen []mcppkg.Server
	status := func(_ string, extra []mcppkg.Server) []mcppkg.ServerStatus {
		seen = extra
		out := make([]mcppkg.ServerStatus, 0, len(extra))
		for _, s := range extra {
			out = append(out, mcppkg.ServerStatus{Name: s.Name, Argv: s.Argv, Source: "project"})
		}
		return out
	}
	ts, application := newMCPTestServer(t, status, config.ProjectAuto)
	sess, err := application.CreateSession(context.Background(), root, "", "", "MCP")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}

	env := requestMCPList(t, ts, string(sess.ID))
	var p protocol.MCPListResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	// auto 模式：项目清单不确认就生效，所以面板必须能看到这台。
	if len(seen) != 1 || seen[0].Name != "repo" {
		t.Fatalf("传给宿主的项目级 server = %+v", seen)
	}
	if len(p.Servers) != 1 || p.Servers[0].Source != "project" {
		t.Fatalf("面板结果 = %+v", p.Servers)
	}
}

func TestMCPListRequiresSession(t *testing.T) {
	status := func(string, []mcppkg.Server) []mcppkg.ServerStatus { return nil }
	ts, _ := newMCPTestServer(t, status, config.ProjectAsk)

	env := requestMCPList(t, ts, "")
	if env.Type != protocol.TypeError {
		t.Fatalf("没有 session_id 时应报错，实际 %s", env.Type)
	}
	var p protocol.ErrorPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码错误: %v", err)
	}
	if p.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s，期望 bad_request", p.Code)
	}
}
