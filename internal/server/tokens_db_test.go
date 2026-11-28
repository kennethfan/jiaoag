package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// newDBTokenServer 起一个接了应用的 server，并把数据库句柄一起返回：
// 令牌以库为准（ADR-0060），测试要直接改库来模拟 `admin token` 的动作。
func newDBTokenServer(t *testing.T, cfg *config.Config) (*httptest.Server, *Server, *store.Store) {
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
	return ts, srv, db
}

func TestDatabaseTokensOverrideConfig(t *testing.T) {
	ctx := context.Background()
	cfg := tokenConfig(t, config.ServerToken{Name: "seed", Value: "seed-token"})
	ts, srv, db := newDBTokenServer(t, cfg)
	url := wsURL(ts)

	// 库为空时保留配置那份，方便首次启动。
	if err := dialErr(url, "seed-token"); err != nil {
		t.Fatalf("库为空时应沿用配置令牌: %v", err)
	}

	if err := db.CreateServerToken(ctx, "ops", HashToken("ops-token"), false); err != nil {
		t.Fatalf("建令牌: %v", err)
	}
	if err := srv.ReloadTokens(ctx); err != nil {
		t.Fatalf("ReloadTokens: %v", err)
	}
	// 库里有记录后，配置里的旧值就不再是真相。
	if err := dialErr(url, "seed-token"); err == nil {
		t.Fatal("库里有令牌后，配置里的旧令牌不该继续有效")
	}
	conn := dial(t, url, "ops-token")
	if env := readEnvelope(t, conn); env.Type != protocol.TypeHello {
		t.Fatalf("首帧应是 hello，实际 %s", env.Type)
	}

	// 轮换：旧值立刻失效，新值可用。
	if err := db.RotateServerToken(ctx, "ops", HashToken("rotated-token")); err != nil {
		t.Fatalf("轮换: %v", err)
	}
	if err := srv.ReloadTokens(ctx); err != nil {
		t.Fatalf("ReloadTokens: %v", err)
	}
	if err := dialErr(url, "ops-token"); err == nil {
		t.Fatal("轮换后旧令牌应失效")
	}
	conn = dial(t, url, "rotated-token")
	if env := readEnvelope(t, conn); env.Type != protocol.TypeHello {
		t.Fatalf("新令牌首帧应是 hello，实际 %s", env.Type)
	}

	// 全部吊销：明确拒绝一切连接，而不是退回配置那份。
	if err := db.RevokeServerToken(ctx, "ops"); err != nil {
		t.Fatalf("吊销: %v", err)
	}
	if err := srv.ReloadTokens(ctx); err == nil {
		t.Fatal("全部吊销时应报告错误（提示所有连接都会被拒）")
	}
	if err := dialErr(url, "rotated-token"); err == nil {
		t.Fatal("全部吊销后任何令牌都不该通过")
	}
	if err := dialErr(url, "seed-token"); err == nil {
		t.Fatal("全部吊销后不该退回配置里的旧令牌")
	}
}

func TestTokenLimiterBuckets(t *testing.T) {
	now := time.Unix(0, 0)
	l := newTokenLimiter(60)
	l.now = func() time.Time { return now }

	// 容量 60，连续 60 次通过，第 61 次拒绝。
	for i := 0; i < 60; i++ {
		if !l.allow("a") {
			t.Fatalf("第 %d 次不该被拒", i+1)
		}
	}
	if l.allow("a") {
		t.Fatal("超过每分钟上限应被拒绝")
	}
	// 另一个令牌有自己的桶。
	if !l.allow("b") {
		t.Fatal("不同令牌应各自计数")
	}
	// 过 1 秒回填 1 个（60/分钟）。
	now = now.Add(time.Second)
	if !l.allow("a") {
		t.Fatal("回填后应重新允许")
	}
	if l.allow("a") {
		t.Fatal("只回填了 1 个，第二次应仍被拒")
	}

	// 0 表示不限。
	if unlimited := newTokenLimiter(0); !unlimited.allow("x") {
		t.Fatal("limit=0 应不限制")
	}
}

func TestWebSocketMessagesAreRateLimited(t *testing.T) {
	limit := 2
	cfg := tokenConfig(t, config.ServerToken{Name: "ops", Value: "ops-token"})
	cfg.Server.TokenRateLimit = &limit
	ts, _ := newTokenServer(t, cfg)
	url := wsURL(ts)

	conn := dial(t, url, "ops-token")
	if env := readEnvelope(t, conn); env.Type != protocol.TypeHello {
		t.Fatalf("首帧应是 hello，实际 %s", env.Type)
	}

	ping := func() protocol.Envelope {
		t.Helper()
		env, err := protocol.New(protocol.TypePing, nil)
		if err != nil {
			t.Fatalf("构造 ping: %v", err)
		}
		write(t, conn, env)
		return readEnvelope(t, conn)
	}

	if got := ping(); got.Type != protocol.TypePong {
		t.Fatalf("第 1 次 ping 应得到 pong，实际 %s", got.Type)
	}
	if got := ping(); got.Type != protocol.TypePong {
		t.Fatalf("第 2 次 ping 应得到 pong，实际 %s", got.Type)
	}
	third := ping()
	if third.Type != protocol.TypeError {
		t.Fatalf("第 3 次应被限速，实际 %s", third.Type)
	}
	var p protocol.ErrorPayload
	if err := third.Decode(&p); err != nil {
		t.Fatalf("解析错误帧: %v", err)
	}
	if p.Code != protocol.CodeRateLimited {
		t.Fatalf("错误码应是 %s，实际 %s", protocol.CodeRateLimited, p.Code)
	}
}
