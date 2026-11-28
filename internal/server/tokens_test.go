package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// newTokenServer 起一个接了应用的 server，并把手柄返回给测试，
// 便于验证运行期换令牌（SIGHUP 走的就是 SetTokens）。
func newTokenServer(t *testing.T, cfg *config.Config) (*httptest.Server, *Server) {
	t.Helper()
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
	return ts, srv
}

func tokenConfig(t *testing.T, tokens ...config.ServerToken) *config.Config {
	t.Helper()
	return &config.Config{
		Server: config.Server{
			Listen: "127.0.0.1:0",
			Tokens: tokens,
		},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
	}
}

// dialErr 尝试连接并返回错误，供「预期被拒」的用例使用。
func dialErr(url, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var header http.Header
	if token != "" {
		header = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	conn.CloseNow()
	return nil
}

func send(t *testing.T, conn *websocket.Conn, typ string, payload any) {
	t.Helper()
	env, err := protocol.New(typ, payload)
	if err != nil {
		t.Fatalf("构造 %s 帧: %v", typ, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, env); err != nil {
		t.Fatalf("发送 %s 帧: %v", typ, err)
	}
}

func TestMultipleTokensAuthorize(t *testing.T) {
	cfg := tokenConfig(t,
		config.ServerToken{Name: "ops", Value: "ops-token"},
		config.ServerToken{Name: "readonly", Value: "ro-token", ReadOnly: true},
	)
	ts, _ := newTokenServer(t, cfg)
	url := wsURL(ts)

	for _, token := range []string{"ops-token", "ro-token"} {
		conn := dial(t, url, token)
		if env := readEnvelope(t, conn); env.Type != protocol.TypeHello {
			t.Fatalf("token %q 首帧是 %s，期望 hello", token, env.Type)
		}
	}
	if err := dialErr(url, "nope"); err == nil {
		t.Fatal("伪造令牌不该连上")
	}
}

func TestReadOnlyTokenCannotWrite(t *testing.T) {
	cfg := tokenConfig(t,
		config.ServerToken{Name: "ro", Value: "ro-token", ReadOnly: true},
	)
	ts, _ := newTokenServer(t, cfg)
	conn := dial(t, wsURL(ts), "ro-token")
	if env := readEnvelope(t, conn); env.Type != protocol.TypeHello {
		t.Fatalf("首帧是 %s", env.Type)
	}
	send(t, conn, protocol.TypeClientHello, protocol.ClientHelloPayload{ClientVersion: "test", ProtocolVersion: protocol.Version})

	// 读命令照常可用。
	send(t, conn, protocol.TypeSessionList, nil)
	if env := readEnvelope(t, conn); env.Type != protocol.TypeSessionListResult {
		t.Fatalf("只读令牌的 session.list 返回 %s", env.Type)
	}

	// 写命令被统一拦在消息循环里。
	for _, typ := range []string{protocol.TypeTaskSubmit, protocol.TypeApprovalResolve, protocol.TypeSessionCreate} {
		send(t, conn, typ, map[string]string{})
		env := readEnvelope(t, conn)
		if env.Type != protocol.TypeError {
			t.Fatalf("%s 应被拒绝，实际回 %s", typ, env.Type)
		}
		var p protocol.ErrorPayload
		if err := env.Decode(&p); err != nil {
			t.Fatalf("解错误帧: %v", err)
		}
		if p.Code != protocol.CodeForbidden {
			t.Fatalf("%s 的错误码是 %s，期望 %s", typ, p.Code, protocol.CodeForbidden)
		}
	}
}

func TestSetTokensHotReload(t *testing.T) {
	cfg := tokenConfig(t, config.ServerToken{Name: "old", Value: "old-token"})
	ts, srv := newTokenServer(t, cfg)
	url := wsURL(ts)

	if err := srv.SetTokens([]Token{{Name: "new", Hash: HashToken("new-token")}}); err != nil {
		t.Fatalf("SetTokens: %v", err)
	}
	if err := dialErr(url, "old-token"); err == nil {
		t.Fatal("旧令牌应在重载后失效")
	}
	conn := dial(t, url, "new-token")
	if env := readEnvelope(t, conn); env.Type != protocol.TypeHello {
		t.Fatalf("新令牌首帧是 %s", env.Type)
	}

	if err := srv.SetTokens(nil); err == nil {
		t.Fatal("空令牌集合应被拒绝")
	}
	// 被拒之后旧集合仍然可用。
	if err := dialErr(url, "new-token"); err != nil {
		t.Fatalf("空集合被拒后新令牌不该失效: %v", err)
	}
}

func TestSetTokensRejectsInvalid(t *testing.T) {
	cfg := tokenConfig(t, config.ServerToken{Name: "ok", Value: "ok-token"})
	_, srv := newTokenServer(t, cfg)

	cases := [][]Token{
		{{Name: "", Hash: "x"}},
		{{Name: "a", Hash: ""}},
		{{Name: "a", Hash: "1"}, {Name: "a", Hash: "2"}},
	}
	for i, tokens := range cases {
		if err := srv.SetTokens(tokens); err == nil {
			t.Fatalf("第 %d 组非法令牌应被拒绝", i)
		}
	}
}
