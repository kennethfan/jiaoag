package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// newModelsServer 起一个配了 providers 的服务端：/models 相关命令只有在
// 配置里能查到供应商时才有意义，newTestServer 的配置没有 providers。
func newModelsServer(t *testing.T, providers ...config.Provider) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Server:    config.Server{Listen: "127.0.0.1:0", Token: testToken},
		Storage:   config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
		Providers: providers,
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
	return ts
}

// modelsSession 建立会话并返回连接与会话 ID。
func modelsSession(t *testing.T, ts *httptest.Server) (*websocket.Conn, string) {
	t.Helper()
	c := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, c) // hello
	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, c, ch)

	create, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: t.TempDir(),
		Title:     "模型测试",
	})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, c, create)
	created := readEnvelope(t, c)
	if created.Type != protocol.TypeSessionCreated {
		t.Fatalf("建会话回帧 = %s，期望 session.created", created.Type)
	}
	var sess protocol.SessionPayload
	if err := created.Decode(&sess); err != nil {
		t.Fatalf("解码 session.created: %v", err)
	}
	return c, sess.ID
}

func TestProviderModelsUsesConfiguredList(t *testing.T) {
	ts := newModelsServer(t, config.Provider{
		Name:    "agnes",
		Type:    "openai",
		BaseURL: "http://127.0.0.1:1", // 不应被请求
		Model:   "m1",
		Models:  []string{"m2", " m1 ", "m2", ""},
		APIKey:  "test-key",
	})
	c, sessionID := modelsSession(t, ts)

	req, err := protocol.New(protocol.TypeProviderModels, protocol.ProviderModelsPayload{SessionID: sessionID})
	if err != nil {
		t.Fatalf("构造 provider.models: %v", err)
	}
	write(t, c, req)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeProviderModelsResult {
		t.Fatalf("回帧 = %s，期望 provider.models.result", env.Type)
	}
	var p protocol.ProviderModelsResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if p.Source != "config" {
		t.Fatalf("来源 = %q，期望 config", p.Source)
	}
	if len(p.Models) != 2 || p.Models[0] != "m2" || p.Models[1] != "m1" {
		t.Fatalf("模型列表 = %v，期望去重后的 [m2 m1]", p.Models)
	}
}

func TestProviderModelsFetchesFromAPI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("请求路径 = %s，期望 /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"b"},{"id":"a"},{"id":"a"}]}`)
	}))
	t.Cleanup(upstream.Close)

	ts := newModelsServer(t, config.Provider{
		Name:    "remote",
		Type:    "openai",
		BaseURL: upstream.URL,
		Model:   "m1",
		APIKey:  "test-key",
	})
	c, sessionID := modelsSession(t, ts)

	req, err := protocol.New(protocol.TypeProviderModels, protocol.ProviderModelsPayload{SessionID: sessionID})
	if err != nil {
		t.Fatalf("构造 provider.models: %v", err)
	}
	write(t, c, req)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeProviderModelsResult {
		t.Fatalf("回帧 = %s，期望 provider.models.result", env.Type)
	}
	var p protocol.ProviderModelsResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if p.Source != "api" {
		t.Fatalf("来源 = %q，期望 api", p.Source)
	}
	if len(p.Models) != 2 || p.Models[0] != "a" || p.Models[1] != "b" {
		t.Fatalf("模型列表 = %v，期望去重排序后的 [a b]", p.Models)
	}
}

func TestSessionUpdateSwitchesModel(t *testing.T) {
	ts := newModelsServer(t,
		config.Provider{Name: "agnes", Type: "openai", BaseURL: "http://127.0.0.1:1", Model: "m1", APIKey: "k"},
		config.Provider{Name: "alt", Type: "openai", BaseURL: "http://127.0.0.1:1", Model: "m9", APIKey: "k"},
	)
	c, sessionID := modelsSession(t, ts)

	upd, err := protocol.New(protocol.TypeSessionUpdate, protocol.SessionUpdatePayload{
		SessionID: sessionID,
		Provider:  "alt",
		Model:     "m3",
	})
	if err != nil {
		t.Fatalf("构造 session.update: %v", err)
	}
	write(t, c, upd)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeSessionUpdated {
		t.Fatalf("回帧 = %s，期望 session.updated", env.Type)
	}
	var p protocol.SessionPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码 session.updated: %v", err)
	}
	if p.Provider != "alt" || p.Model != "m3" {
		t.Fatalf("会话字段 = %s/%s，期望 alt/m3", p.Provider, p.Model)
	}
}

func TestSessionUpdateRejectsUnknownProvider(t *testing.T) {
	ts := newModelsServer(t,
		config.Provider{Name: "agnes", Type: "openai", BaseURL: "http://127.0.0.1:1", Model: "m1", APIKey: "k"},
	)
	c, sessionID := modelsSession(t, ts)

	upd, err := protocol.New(protocol.TypeSessionUpdate, protocol.SessionUpdatePayload{
		SessionID: sessionID,
		Provider:  "nope",
		Model:     "m3",
	})
	if err != nil {
		t.Fatalf("构造 session.update: %v", err)
	}
	write(t, c, upd)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeError {
		t.Fatalf("回帧 = %s，期望 error", env.Type)
	}
	var p protocol.ErrorPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码 error: %v", err)
	}
	if p.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s，期望 %s", p.Code, protocol.CodeBadRequest)
	}
}
