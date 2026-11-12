// Package server 提供 HTTP/WS 入口与连接注册表。
//
// M0 只负责：/healthz、鉴权、WS 握手和心跳。会话与任务调度在 M1 之后接入。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// Server 持有配置、连接集合与 HTTP 服务。
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	version   string
	token     string
	startedAt time.Time
	httpSrv   *http.Server
	app       *app.App

	mu    sync.Mutex
	conns map[*clientConn]struct{}
}

// clientConn 包一层写锁：websocket 的连接允许并发读，但写必须串行。
type clientConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	remote  string
	version string

	// subs 记录本连接上每个会话当前有效的推送协程。同一会话重复订阅时必须
	// 先取消旧的，否则两条协程会各自回放并实时推送，客户端收到重复的 seq。
	subsMu sync.Mutex
	subs   map[string]*streamHandle
}

// streamHandle 是一条推送协程的句柄，用指针身份区分同一会话上的新旧订阅。
type streamHandle struct{ cancel context.CancelFunc }

// beginStream 登记一条推送协程，并取消同一会话上更早的那条。
func (c *clientConn) beginStream(sessionKey string, cancel context.CancelFunc) *streamHandle {
	h := &streamHandle{cancel: cancel}
	c.subsMu.Lock()
	if c.subs == nil {
		c.subs = make(map[string]*streamHandle)
	}
	if prev := c.subs[sessionKey]; prev != nil {
		prev.cancel()
	}
	c.subs[sessionKey] = h
	c.subsMu.Unlock()
	return h
}

// endStream 注销推送协程。只有登记的还是自己时才删除，避免迟到的旧协程
// 把新订阅从表里抹掉。
func (c *clientConn) endStream(sessionKey string, h *streamHandle) {
	c.subsMu.Lock()
	if c.subs[sessionKey] == h {
		delete(c.subs, sessionKey)
	}
	c.subsMu.Unlock()
	h.cancel()
}

func (c *clientConn) send(ctx context.Context, env protocol.Envelope) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wsjson.Write(wctx, c.conn, env)
}

func (c *clientConn) sendError(ctx context.Context, code, msg string) {
	env, err := protocol.New(protocol.TypeError, protocol.ErrorPayload{Code: code, Message: msg})
	if err != nil {
		return
	}
	env.TS = time.Now().UnixMilli()
	_ = c.send(ctx, env)
}

// New 构造服务端；token 在这里解析失败会直接返回错误，避免带着空 token 启动。
func New(cfg *config.Config, log *slog.Logger, version string) (*Server, error) {
	token, err := cfg.Server.Token.Resolve()
	if err != nil {
		return nil, fmt.Errorf("解析 server.token 失败: %w", err)
	}
	s := &Server{
		cfg:       cfg,
		log:       log,
		version:   version,
		token:     token,
		startedAt: time.Now(),
		conns:     make(map[*clientConn]struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWS)
	s.httpSrv = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Handler 暴露 HTTP 路由，供测试与嵌入式启动使用。
func (s *Server) Handler() http.Handler { return s.httpSrv.Handler }

// WithApp 注入应用层。未注入时会话相关消息一律返回 unsupported_type，
// 这样 M0 的握手测试不需要构造数据库。
func (s *Server) WithApp(a *app.App) *Server {
	s.app = a
	return s
}

// ListenAndServe 阻塞运行直到 ctx 取消或监听失败。
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.cfg.Server.Listen, err)
	}
	s.log.Info("服务端已启动",
		"listen", ln.Addr().String(),
		"version", s.version,
		"protocol", protocol.Version,
	)

	errc := make(chan error, 1)
	go func() { errc <- s.httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		s.log.Info("正在关闭服务端")
		s.closeAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"version":          s.version,
		"protocol_version": protocol.Version,
		"uptime_seconds":   int64(time.Since(s.startedAt).Seconds()),
		"connections":      s.connCount(),
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   protocol.CodeUnauthorized,
			"message": "缺少或错误的 Authorization: Bearer <token>",
		})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"localhost", "127.0.0.1"},
	})
	if err != nil {
		s.log.Warn("WS 握手失败", "remote", r.RemoteAddr, "err", err)
		return
	}
	conn.SetReadLimit(1 << 20)

	c := &clientConn{conn: conn, remote: r.RemoteAddr}
	s.addConn(c)
	defer func() {
		s.removeConn(c)
		conn.CloseNow()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	hello, err := protocol.New(protocol.TypeHello, protocol.HelloPayload{
		ServerVersion:   s.version,
		ProtocolVersion: protocol.Version,
		StartedAt:       s.startedAt.UnixMilli(),
		Listen:          s.cfg.Server.Listen,
	})
	if err != nil {
		s.log.Error("构造 hello 失败", "err", err)
		return
	}
	hello.TS = time.Now().UnixMilli()
	if err := c.send(ctx, hello); err != nil {
		return
	}

	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			s.log.Debug("连接结束", "remote", c.remote, "err", err)
			return
		}
		switch env.Type {
		case protocol.TypeClientHello:
			var p protocol.ClientHelloPayload
			if err := env.Decode(&p); err != nil {
				c.sendError(ctx, protocol.CodeBadRequest, err.Error())
				continue
			}
			if p.ProtocolVersion != protocol.Version {
				c.sendError(ctx, protocol.CodeProtocolMismatch, fmt.Sprintf(
					"协议版本不匹配：服务端 %d，客户端 %d", protocol.Version, p.ProtocolVersion))
				_ = conn.Close(websocket.StatusPolicyViolation, "protocol mismatch")
				return
			}
			c.version = p.ClientVersion
			s.log.Info("客户端已连接", "remote", c.remote, "client_version", p.ClientVersion)

		case protocol.TypePing:
			pong, err := protocol.New(protocol.TypePong, nil)
			if err != nil {
				continue
			}
			pong.TS = time.Now().UnixMilli()
			if err := c.send(ctx, pong); err != nil {
				return
			}

		case protocol.TypeSessionCreate:
			s.handleSessionCreate(ctx, c, env)

		case protocol.TypeSessionList:
			s.handleSessionList(ctx, c)

		case protocol.TypeTaskSubmit:
			s.handleTaskSubmit(ctx, c, env)

		case protocol.TypeTaskCancel:
			s.handleTaskCancel(ctx, c, env)

		case protocol.TypeApprovalResolve:
			s.handleApprovalResolve(ctx, c, env)

		case protocol.TypeEventSubscribe:
			s.handleEventSubscribe(ctx, c, env)

		default:
			c.sendError(ctx, protocol.CodeUnsupported, "不支持的消息类型: "+env.Type)
		}
	}
}

// authorized 只接受 Bearer token，且用常数时间比较。
func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) addConn(c *clientConn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeConn(c *clientConn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) closeAll() {
	s.mu.Lock()
	conns := make([]*clientConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
