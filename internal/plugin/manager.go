package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	// 启动超时与单次调用超时。插件是本地进程，启动慢通常意味着它在做重活，
	// 10s 足够；调用超时由配置覆盖。
	defaultStartupTimeout = 10 * time.Second
	defaultCallTimeout    = 60 * time.Second

	// maxToolsPerPlugin 防止一个坏插件把工具表撑爆。
	maxToolsPerPlugin = 200

	// nameMaxBytes 是工具名长度上限，和 MCP 保持一致。
	nameMaxBytes = 64
)

// Server 是一台插件进程的配置。插件是可执行文件，与宿主同权限——不是沙箱。
type Server struct {
	Name string
	Argv []string
	Env  []string
	Dir  string
	// Timeout 是单次 prepare/execute 的等待上限。
	Timeout time.Duration
}

// Options 是 Manager 的构造参数。
type Options struct {
	Servers []Server
	Logger  *slog.Logger
	// Version 是宿主版本，握手时告知插件。
	Version string
	// Dial 仅供测试注入；为空时按 Server.Argv 起子进程。
	Dial func(ctx context.Context, srv Server) (io.ReadWriteCloser, error)
}

type rootState struct {
	mu    sync.Mutex
	ready bool
	tools []tools.Tool
}

// Manager 按工作区懒启动插件：同一个 root 只连一次，失败只记日志。
//
// 关闭归 Manager：插件进程跨任务复用，按任务装配的注册表不能杀它。
type Manager struct {
	servers []Server
	log     *slog.Logger
	version string
	dial    func(ctx context.Context, srv Server) (io.ReadWriteCloser, error)

	mu      sync.Mutex
	roots   map[string]*rootState
	clients []*client
	closed  bool
}

// NewManager 构造插件管理器。
func NewManager(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	dial := opts.Dial
	if dial == nil {
		dial = commandDial
	}
	return &Manager{
		servers: opts.Servers,
		log:     log,
		version: opts.Version,
		dial:    dial,
		roots:   make(map[string]*rootState),
	}
}

// Tools 返回该工作区下所有插件工具（已连接成功的）。
func (m *Manager) Tools(ctx context.Context, ws *workspace.Workspace, env tools.RuntimeEnv) []tools.Tool {
	if m == nil || ws == nil {
		return nil
	}
	root := ws.Root()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	st := m.roots[root]
	if st == nil {
		st = &rootState{}
		m.roots[root] = st
	}
	m.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ready {
		st.tools = m.connectAll(ctx, ws, env)
		st.ready = true
	}
	return append([]tools.Tool(nil), st.tools...)
}

// Source 把插件包装成 tools.Source，由注册表按来源聚合。
func (m *Manager) Source(ws *workspace.Workspace, env tools.RuntimeEnv) tools.Source {
	return &pluginSource{m: m, ws: ws, env: env}
}

type pluginSource struct {
	m   *Manager
	ws  *workspace.Workspace
	env tools.RuntimeEnv
}

func (s *pluginSource) Name() string { return "plugin" }

func (s *pluginSource) Tools(ctx context.Context) ([]tools.Tool, error) {
	return s.m.Tools(ctx, s.ws, s.env), nil
}

// Close 不关插件：进程归 Manager.Close。
func (s *pluginSource) Close() error { return nil }

// Close 关闭所有插件进程，可重复调用。
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	clients := m.clients
	m.clients = nil
	m.roots = make(map[string]*rootState)
	m.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
	return nil
}

func (m *Manager) connectAll(ctx context.Context, ws *workspace.Workspace, env tools.RuntimeEnv) []tools.Tool {
	var out []tools.Tool
	seen := make(map[string]bool)
	for _, srv := range m.servers {
		if len(srv.Argv) == 0 {
			m.log.Warn("插件缺少启动命令，已跳过", "plugin", srv.Name)
			continue
		}
		startCtx, cancel := context.WithTimeout(ctx, defaultStartupTimeout)
		cl, info, err := m.connect(startCtx, srv)
		cancel()
		if err != nil {
			m.log.Warn("插件不可用，已跳过", "plugin", srv.Name, "err", err)
			continue
		}
		list, err := cl.listTools(ctx)
		if err != nil {
			m.log.Warn("插件列工具失败，已跳过", "plugin", srv.Name, "err", err)
			cl.close()
			continue
		}
		if len(list) > maxToolsPerPlugin {
			m.log.Warn("插件工具过多，已截断", "plugin", srv.Name, "got", len(list), "limit", maxToolsPerPlugin)
			list = list[:maxToolsPerPlugin]
		}
		kept := 0
		for _, spec := range list {
			name := spec.Name
			if strings.TrimSpace(name) == "" {
				m.log.Error("插件工具缺少名字，已跳过", "plugin", srv.Name)
				continue
			}
			display := displayName(srv.Name, name)
			if seen[display] {
				m.log.Error("插件工具重名，已跳过", "plugin", srv.Name, "tool", display)
				continue
			}
			seen[display] = true
			out = append(out, newNodeTool(srv.Name, info.Version, name, display, spec, cl, ws, env, m.log, srv.Timeout))
			kept++
		}
		m.log.Info("插件已就绪", "plugin", srv.Name, "tools", kept)
	}
	return out
}

func (m *Manager) connect(ctx context.Context, srv Server) (*client, Info, error) {
	rwc, err := m.dial(ctx, srv)
	if err != nil {
		return nil, Info{}, err
	}
	cl := newClient(rwc)
	// 握手时子进程可能还没读完 stdio，用启动超时的 ctx 兜住。
	info, err := cl.initialize(ctx, m.version)
	if err != nil {
		cl.close()
		return nil, Info{}, err
	}
	m.mu.Lock()
	m.clients = append(m.clients, cl)
	m.mu.Unlock()
	return cl, info, nil
}

// commandDial 起子进程并接上 stdin/stdout。
func commandDial(ctx context.Context, srv Server) (io.ReadWriteCloser, error) {
	cmd := exec.Command(srv.Argv[0], srv.Argv[1:]...)
	cmd.Dir = srv.Dir
	if len(srv.Env) > 0 {
		cmd.Env = append(os.Environ(), srv.Env...)
	}
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: 建立 stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: 建立 stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: 启动 %s: %w", srv.Name, err)
	}
	conn := &cmdConn{cmd: cmd, in: in, out: out}
	go func() {
		// 等进程退出，避免僵尸。Wait 也可能被 Close 触发，用 Once 保护。
		conn.wait()
	}()
	return conn, nil
}

type cmdConn struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *cmdConn) wait() {
	c.once.Do(func() { _ = c.cmd.Wait() })
}

func (c *cmdConn) Close() error {
	c.in.Close()
	c.out.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.wait()
	return nil
}

// displayName 生成 plugin__<server>__<tool>，与 MCP 的命名习惯一致。
func displayName(server, tool string) string {
	name := "plugin__" + sanitize(server) + "__" + sanitize(tool)
	if len(name) <= nameMaxBytes {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:nameMaxBytes-9] + "_" + hex.EncodeToString(sum[:4])
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "plugin"
	}
	return out
}

// schemaBytes 把插件给的 schema 转成注册表要的形态：必须是 JSON object。
func schemaBytes(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

var errNoBackend = errors.New("plugin: 宿主没有可用的执行后端")
