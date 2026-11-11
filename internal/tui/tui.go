// Package tui 是 jiaoag 的终端客户端。
//
// M0 只做：连接、握手、心跳回环与日志行；会话与审批面板在 M1 之后接入。
package tui

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/version"
)

// Options 是连接参数。
type Options struct {
	URL   string
	Token string
}

type (
	connectedMsg struct {
		conn  *websocket.Conn
		hello protocol.HelloPayload
	}
	connectErrMsg struct{ err error }
	envelopeMsg   struct{ env protocol.Envelope }
	closedMsg     struct{ err error }
)

// Model 是 bubbletea 的顶层模型。
type Model struct {
	opts     Options
	conn     *websocket.Conn
	status   string
	lines    []string
	maxLines int
}

// New 构造客户端模型。
func New(opts Options) *Model {
	return &Model{opts: opts, status: "连接中…", maxLines: 500}
}

// Init 立即发起连接。
func (m *Model) Init() tea.Cmd { return m.connectCmd() }

func (m *Model) connectCmd() tea.Cmd {
	opts := m.opts
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		conn, hello, err := Dial(ctx, opts)
		if err != nil {
			return connectErrMsg{err}
		}
		// 主动 ping 一次：M0 的验收点就是这条来回。
		ping, _ := protocol.New(protocol.TypePing, nil)
		_ = wsjson.Write(ctx, conn, ping)

		return connectedMsg{conn: conn, hello: hello}
	}
}

// Dial 建立连接并完成握手。它与模型共用同一段逻辑，同时让 `-check` 能在
// 没有终端的场景（CI、脚本）里验证两端是否真的互通。
func Dial(ctx context.Context, opts Options) (*websocket.Conn, protocol.HelloPayload, error) {
	conn, _, err := websocket.Dial(ctx, opts.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + opts.Token}},
	})
	if err != nil {
		return nil, protocol.HelloPayload{}, fmt.Errorf("连接 %s 失败: %w", opts.URL, err)
	}
	conn.SetReadLimit(1 << 20)

	var env protocol.Envelope
	if err := wsjson.Read(ctx, conn, &env); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("读取 hello 失败: %w", err)
	}
	if env.Type != protocol.TypeHello {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("期待 hello，收到 %s", env.Type)
	}
	var hello protocol.HelloPayload
	if err := env.Decode(&hello); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, err
	}
	if hello.ProtocolVersion != protocol.Version {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("协议版本不匹配：服务端 %d，客户端 %d", hello.ProtocolVersion, protocol.Version)
	}

	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   version.Version,
		ProtocolVersion: protocol.Version,
	})
	if err == nil {
		_ = wsjson.Write(ctx, conn, ch)
	}
	return conn, hello, nil
}

func waitForEnvelope(conn *websocket.Conn) tea.Cmd {
	return func() tea.Msg {
		var env protocol.Envelope
		if err := wsjson.Read(context.Background(), conn, &env); err != nil {
			return closedMsg{err}
		}
		return envelopeMsg{env}
	}
}

// Update 处理按键、连接结果与后续帧。
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.Keystroke() {
		case "q", "ctrl+c", "esc":
			if m.conn != nil {
				_ = m.conn.Close(websocket.StatusNormalClosure, "bye")
			}
			return m, tea.Quit
		case "r":
			if m.conn != nil {
				m.conn.CloseNow()
				m.conn = nil
			}
			m.status = "重连中…"
			m.lines = nil
			return m, m.connectCmd()
		}

	case connectedMsg:
		m.conn = msg.conn
		m.status = fmt.Sprintf("已连接 %s · 服务端 %s · 协议 v%d",
			m.opts.URL, msg.hello.ServerVersion, msg.hello.ProtocolVersion)
		m.append(fmt.Sprintf("握手成功 server=%s protocol=%d", msg.hello.ServerVersion, msg.hello.ProtocolVersion))
		return m, waitForEnvelope(m.conn)

	case envelopeMsg:
		m.append(describe(msg.env))
		return m, waitForEnvelope(m.conn)

	case closedMsg:
		m.status = "连接已断开（r 重连 / q 退出）"
		m.append("连接断开：" + errText(msg.err))
		return m, nil

	case connectErrMsg:
		m.status = "连接失败（r 重试 / q 退出）"
		m.append(errText(msg.err))
		return m, nil
	}
	return m, nil
}

// View 渲染状态行与日志。
func (m *Model) View() tea.View {
	var b strings.Builder
	fmt.Fprintf(&b, "jiaoag %s\n", version.Version)
	fmt.Fprintf(&b, "状态：%s\n", m.status)
	b.WriteString(strings.Repeat("─", 64))
	b.WriteByte('\n')
	b.WriteString(strings.Join(m.lines, "\n"))
	b.WriteString("\n\n[q] 退出  [r] 重连\n")
	return tea.NewView(b.String())
}

func (m *Model) append(line string) {
	m.lines = append(m.lines, fmt.Sprintf("%s %s", time.Now().Format("15:04:05"), line))
	if len(m.lines) > m.maxLines {
		m.lines = m.lines[len(m.lines)-m.maxLines:]
	}
}

func describe(env protocol.Envelope) string {
	switch env.Type {
	case protocol.TypePong:
		return "← pong（双向链路正常）"
	case protocol.TypeError:
		var p protocol.ErrorPayload
		_ = env.Decode(&p)
		return fmt.Sprintf("← error %s: %s", p.Code, p.Message)
	case protocol.TypeEvent:
		return fmt.Sprintf("← event session=%s seq=%d", env.SessionID, env.Seq)
	default:
		return "← " + env.Type
	}
}

func errText(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}
