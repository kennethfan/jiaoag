// Package tui 是 jiaoag 的终端客户端。
//
// M3 起它做三件事：自动建会话并订阅事件、把用户输入提交成任务、把流式
// 增量渲染成对话。渲染函数都是纯函数，便于在无终端环境里断言。
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/version"
)

// Options 是连接参数与建会话参数。
type Options struct {
	URL       string
	Token     string
	Workspace string
	Provider  string
	Model     string
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
	opts   Options
	conn   *websocket.Conn
	status string

	sessionID  string
	taskID     string
	taskStatus string
	round      int
	tokens     int

	dialog   []string
	stream   string
	input    string
	maxLines int

	// approval 非空表示有一个等裁决的变更提案：此时输入行让位给 diff 面板，
	// 除翻页与 y/a/n 之外的按键一律忽略，避免用户一边审 diff 一边打字。
	approval *approvalView
}

// approvalView 是审批面板的本地状态。
type approvalView struct {
	id        string
	path      string
	diff      string
	truncated bool
	// added/removed 是 diff 的规模，来自 patch.Stats：面板头上一行就能
	// 让人知道这次改动有多大，不必先翻到底。
	added   int
	removed int
	offset  int
}

// 审批面板一次显示多少行 diff。
const (
	diffPanelHeight = 18
	diffPageStep    = diffPanelHeight - 2
)

// New 构造客户端模型。
func New(opts Options) *Model {
	if opts.Workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Workspace = wd
		}
	}
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
		return connectedMsg{conn: conn, hello: hello}
	}
}

// Dial 建立连接并完成握手。
func Dial(ctx context.Context, opts Options) (*websocket.Conn, protocol.HelloPayload, error) {
	conn, _, err := websocket.Dial(ctx, opts.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + opts.Token}},
	})
	if err != nil {
		return nil, protocol.HelloPayload{}, fmt.Errorf("连接 %s 失败: %w", opts.URL, err)
	}
	conn.SetReadLimit(4 << 20)

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

// sendCmd 在后台写一帧，避免阻塞 Update。
func sendCmd(conn *websocket.Conn, typ string, payload any) tea.Cmd {
	return func() tea.Msg {
		env, err := protocol.New(typ, payload)
		if err != nil {
			return connectErrMsg{err}
		}
		// 断线后 Model.conn 会被清空：这里必须挡住 nil，
		// 否则 wsjson.Write 的空指针会把整个客户端崩掉。
		if conn == nil {
			return closedMsg{err: errors.New("未连接到服务端")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := wsjson.Write(ctx, conn, env); err != nil {
			return closedMsg{err}
		}
		return nil
	}
}

// Update 处理按键、连接结果与后续帧。
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case connectedMsg:
		m.conn = msg.conn
		m.status = fmt.Sprintf("已连接 %s · 服务端 %s", m.opts.URL, msg.hello.ServerVersion)
		m.dialog = nil
		m.sessionID, m.taskID, m.taskStatus, m.stream = "", "", "", ""
		m.approval = nil
		return m, tea.Batch(
			waitForEnvelope(m.conn),
			sendCmd(m.conn, protocol.TypeSessionCreate, protocol.SessionCreatePayload{
				Workspace: m.opts.Workspace,
				Provider:  m.opts.Provider,
				Model:     m.opts.Model,
			}),
		)

	case envelopeMsg:
		next := m.handleEnvelope(msg.env)
		return m, tea.Batch(next, waitForEnvelope(m.conn))

	case closedMsg:
		// 连接已死，留着指针只会让后续 sendCmd 往一条废连接上写。
		m.conn = nil
		m.status = "连接已断开（ctrl+r 重连 / ctrl+c 退出）"
		m.appendLine("连接断开：" + errText(msg.err))
		return m, nil

	case connectErrMsg:
		m.status = "连接失败（ctrl+r 重试 / ctrl+c 退出）"
		m.appendLine(errText(msg.err))
		return m, nil
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Keystroke()
	// ctrl+c 永远退出：审批面板也不能把用户困在界面里。
	if key == "ctrl+c" {
		m.close()
		return m, tea.Quit
	}
	if m.approval != nil {
		return m.handleApprovalKey(key)
	}
	switch key {
	case "esc":
		// 有任务在跑时 esc 是取消，否则退出：避免误按把界面关掉。
		if m.cancelable() {
			return m, sendCmd(m.conn, protocol.TypeTaskCancel, protocol.TaskCancelPayload{
				SessionID: m.sessionID,
				TaskID:    m.taskID,
			})
		}
		m.close()
		return m, tea.Quit
	case "ctrl+r":
		m.close()
		m.status = "重连中…"
		return m, m.connectCmd()
	case "enter":
		text := strings.TrimSpace(m.input)
		if text == "" || m.sessionID == "" {
			return m, nil
		}
		if m.cancelable() {
			m.appendLine("当前任务还在运行，先按 esc 取消")
			return m, nil
		}
		m.input = ""
		m.appendLine("› " + text)
		return m, sendCmd(m.conn, protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{
			SessionID: m.sessionID,
			Text:      text,
		})
	case "backspace":
		if m.input != "" {
			_, size := utf8.DecodeLastRuneInString(m.input)
			m.input = m.input[:len(m.input)-size]
		}
		return m, nil
	case "space":
		m.input += " "
		return m, nil
	}
	if utf8.RuneCountInString(key) == 1 {
		m.input += key
	}
	return m, nil
}

func (m *Model) handleEnvelope(env protocol.Envelope) tea.Cmd {
	switch env.Type {
	case protocol.TypeSessionCreated:
		var p protocol.SessionPayload
		if err := env.Decode(&p); err == nil {
			m.sessionID = p.ID
			m.status = fmt.Sprintf("会话 %s · %s", p.ID, p.Workspace)
			m.appendLine("已进入会话 " + p.ID)
		}
		return sendCmd(m.conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
			SessionID: m.sessionID,
			FromSeq:   0,
		})

	case protocol.TypeTaskQueued:
		var p protocol.TaskPayload
		if err := env.Decode(&p); err == nil {
			m.taskID, m.taskStatus = p.ID, p.Status
			m.stream = ""
		}
		return nil

	case protocol.TypeTaskCanceled:
		// 回执只表示「取消请求已被接受」：运行中的任务由服务端 agent 收尾，
		// 此刻可能仍是 running。终态以 task.status 事件为准，这里不能改本地状态，
		// 否则 cancelable() 提前变 false，用户会撞上 session_busy。
		m.appendLine("已请求取消（等终态确认）")
		return nil

	case protocol.TypeError:
		var p protocol.ErrorPayload
		_ = env.Decode(&p)
		m.appendLine(fmt.Sprintf("错误 %s：%s", p.Code, p.Message))
		return nil

	case protocol.TypeEvent:
		return m.handleEvent(env)
	}
	return nil
}

func (m *Model) handleEvent(env protocol.Envelope) tea.Cmd {
	var p protocol.StreamEventPayload
	if err := env.Decode(&p); err != nil {
		return nil
	}
	if env.Seq == 0 && p.Kind == string(domain.EventAssistantDelta) {
		// 临时帧：只拼到当前流式段落，不进历史。
		var delta domain.AssistantDeltaPayload
		if err := json.Unmarshal(p.Payload, &delta); err == nil {
			m.stream += delta.Text
		}
		return nil
	}
	if p.TaskID != "" {
		m.taskID = p.TaskID
	}
	switch p.Kind {
	case string(domain.EventTaskStatus):
		var s domain.TaskStatusPayload
		if err := json.Unmarshal(p.Payload, &s); err == nil {
			m.taskStatus = string(s.To)
			if s.To == domain.TaskFailed && s.Error != "" {
				m.appendLine("失败：" + s.Error)
			}
			if s.To == domain.TaskCompleted || s.To == domain.TaskFailed || s.To == domain.TaskCanceled {
				m.stream = ""
			}
			if s.PromptTok+s.OutputTok > 0 {
				m.tokens = s.PromptTok + s.OutputTok
			}
		}
		m.appendLine(describeEvent(p.Kind, p.Payload))
	case string(domain.EventAssistantStart):
		// 上下文超限重试会重新开始一轮流式，先把上一段半截文本清掉。
		m.stream = ""
	case string(domain.EventRoundStarted):
		var r domain.RoundStartedPayload
		if err := json.Unmarshal(p.Payload, &r); err == nil {
			m.round = r.Round
		}
	case string(domain.EventAssistantMessage):
		var a domain.AssistantMessagePayload
		if err := json.Unmarshal(p.Payload, &a); err == nil {
			m.stream = ""
			if a.Text != "" {
				m.appendLine("jiaoag: " + a.Text)
			}
			for _, call := range a.ToolCalls {
				m.appendLine(fmt.Sprintf("  ⚙ %s %s", call.Name, compact(string(call.Args))))
			}
		}
	case string(domain.EventToolResult):
		var t domain.ToolResultPayload
		if err := json.Unmarshal(p.Payload, &t); err == nil {
			status := "完成"
			if t.IsError {
				status = "出错"
			}
			m.appendLine(fmt.Sprintf("  ↳ %s %s（%d ms）", t.Tool, status, t.DurationMS))
		}
	case string(domain.EventApprovalRequested):
		var a domain.ApprovalRequestedPayload
		if err := json.Unmarshal(p.Payload, &a); err == nil {
			added, removed := patch.Stats(a.Diff)
			m.approval = &approvalView{
				id:        string(a.ApprovalID),
				path:      a.Path,
				diff:      a.Diff,
				truncated: a.Truncated,
				added:     added,
				removed:   removed,
			}
			// 面板占屏，提示行要短：详情在下面几行。
			m.appendLine("⚠ 待审批：" + a.Path)
		}
	case string(domain.EventApprovalResolved):
		var a domain.ApprovalResolvedPayload
		if err := json.Unmarshal(p.Payload, &a); err == nil {
			if m.approval != nil && m.approval.id == string(a.ApprovalID) {
				m.approval = nil
			}
			m.appendLine("— 审批 " + decisionLabel(a.Decision))
		}
	case string(domain.EventProposalApplied):
		var p2 domain.ProposalAppliedPayload
		if err := json.Unmarshal(p.Payload, &p2); err == nil {
			m.appendLine("✔ 已写入 " + p2.Path)
		}
	case string(domain.EventProposalRejected):
		var p2 domain.ProposalRejectedPayload
		if err := json.Unmarshal(p.Payload, &p2); err == nil {
			m.appendLine(fmt.Sprintf("✖ 未写入 %s（%s）", p2.Path, p2.Status))
		}
	case string(domain.EventError):
		var e domain.ErrorPayload
		if err := json.Unmarshal(p.Payload, &e); err == nil {
			m.appendLine("错误：" + e.Message)
		}
	}
	return nil
}

// View 渲染状态行、对话与输入行。
func (m *Model) View() tea.View {
	var b strings.Builder
	fmt.Fprintf(&b, "jiaoag %s  %s\n", version.Version, m.status)
	fmt.Fprintf(&b, "任务 %s · 状态 %s · 第 %d 轮 · token %d\n", shortID(m.taskID), orDash(m.taskStatus), m.round, m.tokens)
	b.WriteString(strings.Repeat("─", 72))
	b.WriteByte('\n')
	b.WriteString(strings.Join(m.dialog, "\n"))

	if m.stream != "" {
		b.WriteString("\njiaoag: ")
		b.WriteString(m.stream)
		b.WriteByte('\n')
	}
	switch {
	case m.approval != nil:
		b.WriteByte('\n')
		b.WriteString(m.renderApprovalPanel())
	case m.sessionID != "":
		b.WriteString("\n› ")
		b.WriteString(m.input)
		b.WriteString("▏\n")
	}
	if m.approval == nil {
		b.WriteString("[enter] 发送  [esc] 取消/退出  [ctrl+r] 重连  [ctrl+c] 退出\n")
	}
	return tea.NewView(b.String())
}

func (m *Model) cancelable() bool {
	return m.taskStatus == string(domain.TaskQueued) || m.taskStatus == string(domain.TaskRunning)
}

// handleApprovalKey 处理审批面板上的按键。
//
// 面板打开时刻意不给输入行留任何按键：用户此刻要判断「这份改动对不对」，
// 误把 y/n 打进对话框会很危险。esc 也不退出——按错一次就丢掉整个会话
// 太贵了，要退出请用 ctrl+c。
func (m *Model) handleApprovalKey(key string) (tea.Model, tea.Cmd) {
	v := m.approval
	total := len(strings.Split(strings.TrimRight(v.diff, "\n"), "\n"))
	switch key {
	case "y":
		return m.resolveApproval(domain.DecisionAllowOnce)
	case "a":
		return m.resolveApproval(domain.DecisionAllowSession)
	case "n":
		return m.resolveApproval(domain.DecisionDeny)
	case "ctrl+r":
		// 断线时面板会一直挂着；不在这儿接住 ctrl+r，用户只剩 ctrl+c 一条路。
		// 重连会建一个新会话，connectedMsg 里会把面板清掉。
		m.approval = nil
		m.close()
		m.status = "重连中…"
		return m, m.connectCmd()
	case "up":
		if v.offset > 0 {
			v.offset--
		}
	case "down":
		if v.offset < total-1 {
			v.offset++
		}
	case "pgup":
		v.offset -= diffPageStep
		if v.offset < 0 {
			v.offset = 0
		}
	case "pgdown":
		v.offset += diffPageStep
		if v.offset > total-1 {
			v.offset = total - 1
		}
	}
	return m, nil
}

func (m *Model) resolveApproval(d domain.Decision) (tea.Model, tea.Cmd) {
	v := m.approval
	if v == nil || m.conn == nil {
		return m, nil
	}
	// 先清面板：裁决请求晚到或失败时，界面不该继续显示「待审批」。
	m.approval = nil
	m.appendLine("→ " + decisionLabel(d) + "（等待服务端确认）")
	return m, sendCmd(m.conn, protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
		SessionID:  m.sessionID,
		ApprovalID: v.id,
		Decision:   string(d),
	})
}

// renderDiff 取出 diff 的 [offset, offset+height) 行，并报告后面还有没有内容。
//
// 纯函数：审批面板的翻页边界可以在没有终端的测试里断言。
func renderDiff(diff string, offset, height int) (lines []string, hasMore bool) {
	all := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if height <= 0 {
		height = diffPanelHeight
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + height
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], end < len(all)
}

// renderApprovalPanel 渲染 diff 面板。
func (m *Model) renderApprovalPanel() string {
	v := m.approval
	var b strings.Builder
	// 截断时 +N −M 只是前 256 KB 的统计，必须写在脸上。
	suffix := ""
	if v.truncated {
		suffix = "，已截断"
	}
	fmt.Fprintf(&b, "✎ 待审批 %s（+%d −%d%s）\n", v.path, v.added, v.removed, suffix)
	b.WriteString(strings.Repeat("─", 72))
	b.WriteByte('\n')

	lines, hasMore := renderDiff(v.diff, v.offset, diffPanelHeight)
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if hasMore {
		b.WriteString("… 还有更多（↓ / pgdn）\n")
	}
	if v.truncated {
		b.WriteString("[diff 过长已截断，批准前请先用编辑器核对]\n")
	}
	b.WriteString(strings.Repeat("─", 72))
	b.WriteByte('\n')
	b.WriteString("[y] 批准一次  [a] 本会话内都批准  [n] 拒绝  [↑↓] 翻行  [pgup/pgdn] 翻页\n")
	b.WriteString("[ctrl+r] 重连  [ctrl+c] 退出\n")
	return b.String()
}

func decisionLabel(d domain.Decision) string {
	switch d {
	case domain.DecisionAllowOnce:
		return "已批准（一次）"
	case domain.DecisionAllowSession:
		return "已批准（本会话内同类请求）"
	case domain.DecisionDeny:
		return "已拒绝"
	case domain.DecisionExpired:
		return "已过期"
	default:
		return string(d)
	}
}

func (m *Model) close() {
	if m.conn != nil {
		_ = m.conn.Close(websocket.StatusNormalClosure, "bye")
		m.conn = nil
	}
}

func (m *Model) appendLine(line string) {
	m.dialog = append(m.dialog, line)
	if len(m.dialog) > m.maxLines {
		m.dialog = m.dialog[len(m.dialog)-m.maxLines:]
	}
}

// describeEvent 只给关心的事件生成一行摘要，其余事件不打扰界面。
func describeEvent(kind string, payload json.RawMessage) string {
	switch kind {
	case string(domain.EventTaskStatus):
		var s domain.TaskStatusPayload
		if err := json.Unmarshal(payload, &s); err != nil {
			return ""
		}
		out := fmt.Sprintf("— %s → %s", s.From, s.To)
		if s.Reason != "" {
			out += "（" + s.Reason + "）"
		}
		return out
	}
	return ""
}

func shortID(id string) string {
	if id == "" {
		return "—"
	}
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func compact(raw string) string {
	// 按 rune 截：参数里的中文按字节切会切出半个字符，终端直接显示乱码。
	if utf8.RuneCountInString(raw) <= 160 {
		return raw
	}
	return string([]rune(raw)[:160]) + "…"
}

func errText(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}
