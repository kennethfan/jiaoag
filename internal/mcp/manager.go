// Package mcp 把 MCP server 暴露的工具接入 jiaoag 的 tools.Registry。
//
// 设计边界（ADR-0014）：
//   - 只支持 stdio transport：一个 server 一个子进程，按 workspace 懒启动；
//   - 工具调用永远走「外部工具」审批通道，宿主拿不到 argv/diff；
//   - server 不可用不是任务失败：记日志、跳过，核心工具照常工作；
//   - 不做自动重连：进程崩了就是这一轮不可用，下次启动任务时重建。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/version"
)

const (
	// defaultStartupTimeout 是 initialize + tools/list 的上限。server 起不来
	// 时任务不能陪着一起挂，超时就当这一台不可用。
	defaultStartupTimeout = 10 * time.Second
	// defaultCallTimeout 是单次 tools/call 的默认上限；配置可按 server 覆盖。
	defaultCallTimeout = 60 * time.Second
	// maxToolNameLen 对齐主流模型的函数名上限（64）。超长名截断后加哈希，
	// 避免两个长工具名截成同一个。
	maxToolNameLen = 64
)

// Server 是一台 stdio MCP server 的启动配置。
type Server struct {
	// Name 是配置里的短名，用于工具前缀与审计来源；只允许 [a-z0-9_-]。
	Name string
	// Argv 是完整启动命令（含参数），不经过 shell。
	Argv []string
	// Env 是追加到宿主环境之上的 KEY=VALUE。
	Env []string
	// Dir 是子进程工作目录；空表示继承 server 进程。
	Dir string
	// Timeout 是单次 tools/call 的上限；零值用 defaultCallTimeout。
	Timeout time.Duration
	// Trust 为真表示本地配置放行（配置即信任，默认），调用免审批但仍记审计；
	// 为假对应 trust: ask，逐次人工批准。
	Trust bool
}

// Options 构造 Manager。Connect 仅用于测试注入 in-memory transport。
type Options struct {
	Servers []Server
	Logger  *slog.Logger
	Connect func(ctx context.Context, s Server) (*sdk.ClientSession, error)
}

// Manager 按 workspace 缓存已连接的 server，避免每个任务重复起进程。
type Manager struct {
	servers []Server
	log     *slog.Logger
	connect func(ctx context.Context, s Server) (*sdk.ClientSession, error)

	mu    sync.Mutex
	roots map[string]*rootSources
}

type rootSources struct {
	once     sync.Once
	tools    []tools.Tool
	sessions []*sdk.ClientSession
}

// NewManager 构造管理器。Manager 不主动连接：第一次 Tools 才启动子进程。
func NewManager(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	connect := opts.Connect
	if connect == nil {
		connect = commandSession
	}
	return &Manager{
		servers: opts.Servers,
		log:     log,
		connect: connect,
		roots:   make(map[string]*rootSources),
	}
}

// Tools 返回该 workspace 可用的 MCP 工具。任何 server 失败都只记日志并
// 跳过：MCP 是增强，不是运行前提。
//
// 返回的切片是副本，调用方可以安全地追加进自己的 registry。
func (m *Manager) Tools(ctx context.Context, workspaceRoot string) []tools.Tool {
	if len(m.servers) == 0 {
		return nil
	}
	m.mu.Lock()
	state := m.roots[workspaceRoot]
	if state == nil {
		state = &rootSources{}
		m.roots[workspaceRoot] = state
	}
	m.mu.Unlock()

	state.once.Do(func() {
		state.tools, state.sessions = m.connectAll(ctx, workspaceRoot)
	})
	out := make([]tools.Tool, len(state.tools))
	copy(out, state.tools)
	return out
}

func (m *Manager) connectAll(ctx context.Context, root string) ([]tools.Tool, []*sdk.ClientSession) {
	var (
		out      []tools.Tool
		sessions []*sdk.ClientSession
		seen     = make(map[string]string, len(m.servers))
	)
	for _, srv := range m.servers {
		if len(srv.Argv) == 0 && m.connect == nil {
			m.log.Warn("MCP server 缺少启动命令，已跳过", "server", srv.Name)
			continue
		}
		if prev, ok := seen[srv.Name]; ok {
			m.log.Error("MCP server 名称重复，已跳过", "server", srv.Name, "first_command", prev)
			continue
		}
		seen[srv.Name] = strings.Join(srv.Argv, " ")

		startCtx, cancel := context.WithTimeout(ctx, defaultStartupTimeout)
		sess, err := m.connect(startCtx, srv)
		cancel()
		if err != nil {
			m.log.Warn("MCP server 启动失败，已跳过", "server", srv.Name, "err", err)
			continue
		}
		sessions = append(sessions, sess)

		toolsForServer, err := m.listTools(ctx, sess, srv, root)
		if err != nil {
			m.log.Warn("MCP server 列出工具失败，已跳过", "server", srv.Name, "err", err)
			continue
		}
		out = append(out, toolsForServer...)
		m.log.Info("MCP server 已就绪", "server", srv.Name, "tools", len(toolsForServer))
	}
	return out, sessions
}

func (m *Manager) listTools(ctx context.Context, sess *sdk.ClientSession, srv Server, root string) ([]tools.Tool, error) {
	serverVersion := version.Version
	if info := sess.InitializeResult(); info != nil && info.ServerInfo != nil {
		serverVersion = info.ServerInfo.Version
	}
	timeout := srv.Timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}

	var out []tools.Tool
	names := make(map[string]string)
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return out, err
		}
		if tool == nil || tool.Name == "" {
			continue
		}
		display := displayName(srv.Name, tool.Name)
		if prev, ok := names[display]; ok {
			m.log.Error("MCP 工具名冲突，已跳过", "name", display, "first", prev, "second", tool.Name)
			continue
		}
		names[display] = tool.Name

		schema, err := schemaBytes(tool.InputSchema)
		if err != nil {
			m.log.Warn("MCP 工具 schema 无法序列化，已跳过",
				"server", srv.Name, "tool", tool.Name, "err", err)
			continue
		}
		desc := strings.TrimSpace(tool.Description)
		if desc == "" {
			desc = fmt.Sprintf("MCP 工具 %s/%s", srv.Name, tool.Name)
		}
		out = append(out, &remoteTool{
			session:    sess,
			remoteName: tool.Name,
			spec: tools.Spec{
				Name:        display,
				Description: desc,
				Schema:      schema,
				Effects:     []tools.Effect{tools.EffectProcess},
				Version:     fmt.Sprintf("mcp:%s@%s#%s", srv.Name, serverVersion, shortHash(schema)),
			},
			source:  "mcp:" + srv.Name,
			trust:   srv.Trust,
			timeout: timeout,
			log:     m.log,
		})
	}
	return out, nil
}

// Close 关闭所有已连接的子进程。可以重复调用。
func (m *Manager) Close() error {
	m.mu.Lock()
	states := make([]*rootSources, 0, len(m.roots))
	for _, s := range m.roots {
		states = append(states, s)
	}
	m.roots = make(map[string]*rootSources)
	m.mu.Unlock()

	var first error
	for _, st := range states {
		for _, sess := range st.sessions {
			if err := sess.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// commandSession 是默认连接方式：起子进程，用 stdin/stdout 上的
// newline-delimited JSON 通信。CommandTransport 负责在 Close 时回收进程。
func commandSession(ctx context.Context, s Server) (*sdk.ClientSession, error) {
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	if len(s.Env) > 0 {
		env := os.Environ()
		env = append(env, s.Env...)
		cmd.Env = env
	}
	// server 自己的诊断信息走宿主 stderr：stdout 是协议通道，不能混。
	cmd.Stderr = os.Stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "jiaoag", Version: version.Version}, nil)
	return client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
}

// displayName 生成模型可见的工具名：mcp__<server>__<tool>。
// 只保留 [A-Za-z0-9_-]，其余替换为 '_'，避免供应商的函数名校验拒绝整轮请求。
func displayName(server, tool string) string {
	name := "mcp__" + sanitizeName(server) + "__" + sanitizeName(tool)
	if len(name) <= maxToolNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:maxToolNameLen-9] + "_" + hex.EncodeToString(sum[:4])
}

func sanitizeName(s string) string {
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
		return "unnamed"
	}
	return out
}

func schemaBytes(schema any) (json.RawMessage, error) {
	if schema == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || raw[0] != '{' {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	return raw, nil
}

func shortHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:4])
}
