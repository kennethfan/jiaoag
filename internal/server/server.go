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

	// idleExit 是「最后一个连接断开后等多久退出」；0 表示常驻。
	// idleC 只关闭一次，作为 ListenAndServe 的退出信号。
	idleExit   time.Duration
	idleC      chan struct{}
	idleTimer  *time.Timer
	idleClosed bool

	// tlsCert / tlsKey 非空时以 TLS 提供服务（wss）。由 main 从配置注入，
	// 与 Handler() 的测试路径无关。
	tlsCert string
	tlsKey  string
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

// SetIdleExit 设置空闲退出时长：最后一个 WS 连接断开后开始倒计时，期间
// 有新连接则取消；超时且没有排队/执行中的任务时关闭服务端。
//
// 0 表示常驻。自动拉起的 TUI 会传 30s，手动启动的 server 不传，行为不变。
func (s *Server) SetIdleExit(d time.Duration) *Server {
	if d < 0 {
		d = 0
	}
	s.idleExit = d
	if d > 0 {
		s.idleC = make(chan struct{})
	}
	return s
}

// SetTLS 让服务端以 TLS 提供服务（wss）。两个参数都为空表示不启用。
// 证书在 config 校验阶段已确认可加载，这里不再重复解析。
func (s *Server) SetTLS(certFile, keyFile string) *Server {
	s.tlsCert, s.tlsKey = certFile, keyFile
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
		"tls", s.tlsCert != "",
	)

	errc := make(chan error, 1)
	go func() {
		if s.tlsCert != "" {
			errc <- s.httpSrv.ServeTLS(ln, s.tlsCert, s.tlsKey)
			return
		}
		errc <- s.httpSrv.Serve(ln)
	}()

	// 启动即开始空闲倒计时：TUI 拉起 server 后如果自己先崩了，没人连接时
	// 不应该留下一个孤儿进程。TUI 正常连接会立刻取消定时器。
	s.mu.Lock()
	if s.connCountLocked() == 0 {
		s.armIdleLocked()
	}
	s.mu.Unlock()

	shutdown := func() error {
		s.log.Info("正在关闭服务端")
		s.closeAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	select {
	case <-ctx.Done():
		return shutdown()
	case <-s.idleC:
		s.log.Info("空闲超时，服务端退出", "idle", s.idleExit)
		return shutdown()
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
		case protocol.TypeSessionUpdate:
			s.handleSessionUpdate(ctx, c, env)
		case protocol.TypeProviderModels:
			s.handleProviderModels(ctx, c, env)
		case protocol.TypeSkillList:
			s.handleSkillList(ctx, c, env)
		case protocol.TypeMCPList:
			s.handleMCPList(ctx, c, env)
		case protocol.TypeAuditTail:
			s.handleAuditTail(ctx, c, env)
		case protocol.TypeEventHead:
			s.handleEventHead(ctx, c, env)
		case protocol.TypeProjectTrust:
			s.handleProjectTrust(ctx, c, env)
		case protocol.TypeProjectRevoke:
			s.handleProjectRevoke(ctx, c, env)

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
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.mu.Unlock()
}

func (s *Server) removeConn(c *clientConn) {
	s.mu.Lock()
	delete(s.conns, c)
	if len(s.conns) == 0 {
		s.armIdleLocked()
	}
	s.mu.Unlock()
}

func (s *Server) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) connCountLocked() int { return len(s.conns) }

// armIdleLocked 在「没有连接」时启动空闲倒计时。调用方必须持有 s.mu。
func (s *Server) armIdleLocked() {
	if s.idleExit <= 0 || s.idleTimer != nil || s.idleClosed {
		return
	}
	s.idleTimer = time.AfterFunc(s.idleExit, s.checkIdle)
}

// checkIdle 是倒计时回调：有新连接就放弃；仍有排队或执行中的任务就再等
// 一轮；否则关闭 idleC，让 ListenAndServe 走正常关闭路径。
func (s *Server) checkIdle() {
	s.mu.Lock()
	s.idleTimer = nil
	if len(s.conns) > 0 || s.idleClosed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	busy := false
	if s.app != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if b, err := s.app.HasBusyTasks(ctx); err != nil {
			s.log.Warn("空闲退出前检查任务失败，暂不退出", "err", err)
			busy = true
		} else {
			busy = b
		}
		cancel()
	}
	if busy {
		s.mu.Lock()
		s.armIdleLocked()
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) > 0 || s.idleClosed {
		return
	}
	s.idleClosed = true
	close(s.idleC)
}

// closeAll 立即断开所有 WS 连接。
//
// 这里刻意不用 conn.Close：它会先发 close 帧再等对端回话，对端不读时（比如
// 卡住的客户端）会一直挂到内部 5 秒超时，把 http.Server.Shutdown 拖满整个
// 宽限期；而那时再调 CloseNow 也没用——Close 已经占住了「正在关闭」状态，
// CloseNow 会直接返回。关闭是终态，直接断，客户端按断线重连处理。
func (s *Server) closeAll() {
	s.mu.Lock()
	conns := make([]*clientConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.conn.CloseNow()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
