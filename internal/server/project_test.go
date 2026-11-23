package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

// newProjectServer 起一个 project.mode=ask 的服务端，工作区里放一份项目提示。
func newProjectServer(t *testing.T) (*httptest.Server, *app.App, string) {
	t.Helper()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "AGENTS.md"), []byte("项目规则：先跑测试。\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0", Token: testToken},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
		Project: config.Project{Mode: config.ProjectAsk},
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
		MaxRounds:     3,
		ContextBudget: 10000,
		ProjectMode:   config.ProjectAsk,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = application.Close()
	})
	return ts, application, wsDir
}

// createSession 走 WS 建会话，返回会话 ID 与 session.created 里的项目信息。
func createProjectSession(t *testing.T, ts *httptest.Server, wsDir string) (string, *protocol.ProjectInfo) {
	t.Helper()
	c := helloConn(t, ts)
	env, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{Workspace: wsDir, Title: "项目级"})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, c, env)
	reply := readEnvelope(t, c)
	if reply.Type != protocol.TypeSessionCreated {
		t.Fatalf("回帧 = %s，期望 session.created", reply.Type)
	}
	var p protocol.SessionPayload
	if err := reply.Decode(&p); err != nil {
		t.Fatalf("解码 session.created: %v", err)
	}
	return p.ID, p.Project
}

func TestSessionCreateReportsDetectedProject(t *testing.T) {
	ts, _, wsDir := newProjectServer(t)
	_, project := createProjectSession(t, ts, wsDir)
	if project == nil {
		t.Fatal("session.created 应带上项目级扩展信息")
	}
	if !project.Detected || project.Loaded || project.Mode != config.ProjectAsk {
		t.Fatalf("未确认时应发现但不加载，得到 %+v", project)
	}
	if project.Summary != "提示 1 个" {
		t.Fatalf("摘要不对：%q", project.Summary)
	}
	if len(project.Prompts) != 1 || project.Prompts[0] != "AGENTS.md" {
		t.Fatalf("提示清单不对：%v", project.Prompts)
	}
}

func TestProjectTrustThenRevokeOverWS(t *testing.T) {
	ts, _, wsDir := newProjectServer(t)
	sessionID, _ := createProjectSession(t, ts, wsDir)
	c := helloConn(t, ts)

	trust, err := protocol.New(protocol.TypeProjectTrust, protocol.ProjectActionPayload{SessionID: sessionID})
	if err != nil {
		t.Fatalf("构造 project.trust: %v", err)
	}
	write(t, c, trust)
	reply := readEnvelope(t, c)
	if reply.Type != protocol.TypeProjectTrusted {
		t.Fatalf("回帧 = %s，期望 project.trusted", reply.Type)
	}
	var status protocol.ProjectStatusPayload
	if err := reply.Decode(&status); err != nil {
		t.Fatalf("解码 project.trusted: %v", err)
	}
	if !status.Project.Trusted || !status.Project.Loaded {
		t.Fatalf("确认后应加载，得到 %+v", status.Project)
	}

	revoke, err := protocol.New(protocol.TypeProjectRevoke, protocol.ProjectActionPayload{SessionID: sessionID})
	if err != nil {
		t.Fatalf("构造 project.revoke: %v", err)
	}
	write(t, c, revoke)
	reply = readEnvelope(t, c)
	if reply.Type != protocol.TypeProjectTrusted {
		t.Fatalf("回帧 = %s，期望 project.trusted", reply.Type)
	}
	if err := reply.Decode(&status); err != nil {
		t.Fatalf("解码撤销结果: %v", err)
	}
	if status.Project.Trusted || status.Project.Loaded {
		t.Fatalf("撤销后不该加载，得到 %+v", status.Project)
	}
}

// 没有项目资产的会话不该带这个字段，客户端也就不必显示提示行。
func TestSessionCreateOmitsProjectWhenEmpty(t *testing.T) {
	ts, _, _ := newProjectServer(t)
	_, project := createProjectSession(t, ts, t.TempDir())
	if project != nil {
		t.Fatalf("空工作区不该有项目信息：%+v", project)
	}
}

// 未知会话要明确报错，而不是静默成功。
func TestProjectTrustRejectsUnknownSession(t *testing.T) {
	ts, _, _ := newProjectServer(t)
	c := helloConn(t, ts)
	env, err := protocol.New(protocol.TypeProjectTrust, protocol.ProjectActionPayload{SessionID: "ses_nope"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, c, env)
	reply := readEnvelope(t, c)
	if reply.Type != protocol.TypeError {
		t.Fatalf("回帧 = %s，期望 error", reply.Type)
	}
	var e protocol.ErrorPayload
	if err := reply.Decode(&e); err != nil {
		t.Fatalf("解码错误: %v", err)
	}
	if e.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s，期望 bad_request", e.Code)
	}
}
