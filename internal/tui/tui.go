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
	"github.com/kennethfan/jiaoag/internal/localshell"
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
	// SessionID 非空时跳过建会话，直接订阅该会话（进程重启后用它回到现场）。
	SessionID string
}

type (
	connectedMsg struct {
		conn  *websocket.Conn
		hello protocol.HelloPayload
	}
	connectErrMsg struct{ err error }
	envelopeMsg   struct{ env protocol.Envelope }
	closedMsg     struct{ err error }
	// localMsg 是 !命令 的一行输出或结束态。
	localMsg struct{ ev localshell.Event }
)

// Model 是 bubbletea 的顶层模型。
type Model struct {
	opts   Options
	conn   *websocket.Conn
	status string

	sessionID string
	// lastSeq 是本地已收到的最后一条持久事件序号。首次连接为 0（全量回放），
	// 断线重连从它之后增量回放；临时流式帧 seq=0，不推进它。
	lastSeq int64
	// runTaskID 是正在执行（或等审批）的任务，esc 只取消它。提交排队任务时
	// 服务端回执里的 id 不能覆盖它，否则 esc 会取消错误的任务。
	runTaskID string
	// queue 是本会话排队中的任务，顺序即服务端 FIFO 执行顺序。它完全由
	// task.queued / task.status 事件重建，重连全量回放时也能还原。
	queue []queuedTask
	// queuePanel 非空表示队列面板打开：此时按键交给面板，不写输入行。
	queuePanel *queuePanelView
	// modelPanel 非空表示 /models 的模型选择面板打开，优先级高于输入行。
	modelPanel      *modelPanelView
	sessionProvider string
	sessionModel    string
	taskID          string
	taskStatus      string
	round           int
	tokens          int

	// local 非空表示有 !命令 在本机执行中。它不经过服务端、不进审计、
	// 不受 exec.backend 沙箱约束；输出只留在对话区，模型看不到。
	local *localshell.Run
	// localLines/localOmitted 统计本次本地命令已渲染/被省略的显示行数。
	localLines   int
	localOmitted int

	dialog   []string
	stream   string
	input    string
	maxLines int

	// approval 非空表示有一个等裁决的变更提案：此时输入行让位给 diff 面板，
	// 除翻页与 y/a/n 之外的按键一律忽略，避免用户一边审 diff 一边打字。
	approval *approvalView
}

// approvalView 是审批面板的本地状态。kind=command 时展示 argv/cwd/后端，
// 不展示 diff。
type approvalView struct {
	id        string
	kind      string
	path      string
	diff      string
	truncated bool
	command   []string
	cwd       string
	backend   string
	timeoutMS int64
	reason    string
	// 外部工具（MCP）没有 diff / argv：面板展示来源、工具名与参数摘要。
	tool   string
	source string
	args   string
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

// panelWidth 是对话区与面板的宽度（与分隔线一致）：超出的行必须折行，
// 否则一条 4 KB 的 argv token 会把整个终端布局冲垮。
const panelWidth = 72

// maxOutputPreviewLines 是命令输出在界面上最多占多少行。全文在日志里，
// 界面只负责让人看懂这次调用发生了什么（失败诊断、超时文案）。
const maxOutputPreviewLines = 12

// outputIndent 是命令类输出在对话区的缩进：工具结果与本地命令共用一套，
// 让人一眼分出「agent 干了什么」和「我敲的东西」。
const outputIndent = "  │ "

// localCommandLines 是一次 !命令 最多占多少显示行（折行后计）。超出后仍
// 继续消费事件但不再渲染，否则子进程会卡在写管道上。
const localCommandLines = 400

// New 构造客户端模型。
func New(opts Options) *Model {
	if opts.Workspace == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Workspace = wd
		}
	}
	return &Model{
		opts:      opts,
		status:    "连接中…",
		maxLines:  500,
		sessionID: opts.SessionID,
	}
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
		if conn == nil {
			// 断线后 Model.conn 已经清空，但可能还有一帧在途（旧读协程已经
			// 交付、只是还没被 Update 处理）。没有这层保护，wsjson.Read 会在
			// nil 连接上解引用并把整个 TUI 打崩。
			return closedMsg{err: errors.New("未连接到服务端")}
		}
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

// startLocalCommand 在客户端本机执行 !命令。
//
// cwd 是 TUI 的工作区，环境变量原样继承：这条路径等价于用户在自己的终端
// 里敲命令，所以不审批、不审计，也不受 exec.backend 沙箱限制。
func (m *Model) startLocalCommand(text string) tea.Cmd {
	command := strings.TrimSpace(strings.TrimPrefix(text, "!"))
	if command == "" {
		m.appendLine("! 后面要跟命令，例如 !go test ./...")
		return nil
	}
	if m.local != nil {
		m.appendLine("上一个本地命令还在跑（ctrl+x 终止）")
		return nil
	}
	run := localshell.Start(localshell.Options{Workdir: m.opts.Workspace}, command)
	m.local = run
	m.localLines, m.localOmitted = 0, 0
	m.appendLine("! " + command)
	return waitLocalCommand(run)
}

// waitLocalCommand 取一条本地输出。每收到一条都要重新发起，直到 Done：
// 读端停下子进程就会阻塞在写管道上。
func waitLocalCommand(run *localshell.Run) tea.Cmd {
	return func() tea.Msg {
		if run == nil {
			return localMsg{ev: localshell.Event{Done: true}}
		}
		ev, ok := <-run.Events()
		if !ok {
			return localMsg{ev: localshell.Event{Done: true}}
		}
		return localMsg{ev: ev}
	}
}

// handleLocalCommand 把一行输出或结束态写进对话区。
func (m *Model) handleLocalCommand(ev localshell.Event) (tea.Model, tea.Cmd) {
	if !ev.Done {
		if m.localLines < localCommandLines {
			width := panelWidth - len([]rune(outputIndent))
			for _, chunk := range wrapText(ev.Text, width) {
				m.appendLine(outputIndent + chunk)
			}
			m.localLines++
		} else {
			// 超出展示上限后只计数、不渲染，但事件照读不误。
			m.localOmitted++
		}
		if m.local == nil {
			return m, nil
		}
		return m, waitLocalCommand(m.local)
	}

	m.local = nil
	omitted := m.localOmitted
	m.localLines, m.localOmitted = 0, 0

	var line string
	switch {
	case ev.TimedOut:
		line = fmt.Sprintf("  ↳ 超时 %s，已终止", shortDuration(ev.Duration))
	case ev.Stopped:
		line = fmt.Sprintf("  ↳ 已终止（%s）", shortDuration(ev.Duration))
	case ev.ExitCode < 0:
		line = fmt.Sprintf("  ↳ 未能执行（%s）", errText(ev.Err))
	default:
		line = fmt.Sprintf("  ↳ 退出码 %d（%s）", ev.ExitCode, shortDuration(ev.Duration))
	}
	if omitted > 0 {
		line += fmt.Sprintf(" · 省略 %d 行", omitted)
	}
	m.appendLine(line)
	return m, nil
}

func shortDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Update 处理按键、连接结果与后续帧。
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case connectedMsg:
		m.conn = msg.conn
		m.status = fmt.Sprintf("已连接 %s · 服务端 %s", m.opts.URL, msg.hello.ServerVersion)
		if m.sessionID != "" {
			// 断线重连 / --session 恢复：从本地最后一条持久事件之后增量回放。
			//
			// 首次连接（lastSeq=0）仍走全量重放。增量时保留历史与队列视图，
			// 但要清掉两样不入库的本地状态：半截流式文本，以及旧审批面板
			// （它可能已在断线期间被裁决，由快照重新填充真正待审批的那些）。
			from := m.lastSeq
			if from == 0 {
				m.resetView()
			} else {
				m.stream = ""
				m.approval = nil
			}
			return m, tea.Batch(
				waitForEnvelope(m.conn),
				sendCmd(m.conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
					SessionID: m.sessionID,
					FromSeq:   from,
				}),
			)
		}
		m.resetView()
		m.sessionID = ""
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
		// 断线可能已经处理过：迟到的事件帧仍要渲染，但不能再拿 nil 连接
		// 续读下一帧。
		if m.conn == nil {
			return m, next
		}
		return m, tea.Batch(next, waitForEnvelope(m.conn))

	case localMsg:
		return m.handleLocalCommand(msg.ev)

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
	// ctrl+x 只终止本机命令：它是用户自己敲的，随时可以叫停，不该被
	// 审批面板或会话状态挡住。
	if key == "ctrl+x" {
		if m.local == nil {
			return m, nil
		}
		m.local.Stop()
		m.appendLine("正在终止本地命令…")
		return m, nil
	}
	if m.approval != nil {
		return m.handleApprovalKey(key)
	}
	if m.queuePanel != nil {
		return m.handleQueueKey(key)
	}
	if m.modelPanel != nil {
		return m.handleModelKey(key)
	}
	switch key {
	case "ctrl+q":
		if len(m.queue) == 0 {
			m.appendLine("队列为空：当前没有排队任务")
			return m, nil
		}
		m.queuePanel = &queuePanelView{}
		return m, nil
	case "esc":
		// 有任务在跑时 esc 是取消，否则退出：避免误按把界面关掉。
		// 只认正在执行/等审批的那个 id，排队任务不能在这里被误取消。
		if m.runTaskID != "" {
			return m, sendCmd(m.conn, protocol.TypeTaskCancel, protocol.TaskCancelPayload{
				SessionID: m.sessionID,
				TaskID:    m.runTaskID,
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
		if text == "" {
			return m, nil
		}
		// !命令 在客户端本机执行：不需要会话，服务端没连上也能用。
		if strings.HasPrefix(text, "!") {
			m.input = ""
			return m, m.startLocalCommand(text)
		}
		if m.sessionID == "" {
			return m, nil
		}
		if text == "/models" {
			m.input = ""
			m.appendLine("正在获取模型列表…")
			return m, sendCmd(m.conn, protocol.TypeProviderModels, protocol.ProviderModelsPayload{
				SessionID: m.sessionID,
			})
		}
		// 运行中也可以提交：服务端会把它排到队尾，会话内仍是串行执行。
		//
		// 提问不回显在这里：它由 task.queued 事件统一渲染，否则断线重连后的
		// 全量重放会缺用户原话，而实时提交又会打两遍。
		m.input = ""
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
	case protocol.TypeEventSnapshot:
		var p protocol.EventSnapshotPayload
		if err := env.Decode(&p); err != nil {
			return nil
		}
		if p.Reset {
			// 本地 seq 超过服务端 head：两边不是同一份日志，只能从头来。
			m.resetView()
			return sendCmd(m.conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
				SessionID: m.sessionID,
				FromSeq:   0,
			})
		}
		for _, ev := range p.Pending {
			if ev.Kind != string(domain.EventApprovalRequested) {
				continue
			}
			var a domain.ApprovalRequestedPayload
			if err := json.Unmarshal(ev.Payload, &a); err == nil {
				// 不写提示行：历史里可能已经有这条请求的提示，面板才是事实。
				m.showApprovalRequest(a, false)
			}
		}
		return nil

	case protocol.TypeSessionCreated:
		var p protocol.SessionPayload
		if err := env.Decode(&p); err == nil {
			m.sessionID = p.ID
			m.sessionProvider, m.sessionModel = p.Provider, p.Model
			m.status = fmt.Sprintf("会话 %s · %s", p.ID, p.Workspace)
			m.appendLine("已进入会话 " + p.ID)
			if p.Model != "" {
				m.appendLine("模型：" + modelLabel(p.Provider, p.Model))
			}
		}
		return sendCmd(m.conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
			SessionID: m.sessionID,
			FromSeq:   0,
		})

	case protocol.TypeSessionUpdated:
		var p protocol.SessionPayload
		if err := env.Decode(&p); err == nil {
			m.sessionProvider, m.sessionModel = p.Provider, p.Model
		}
		return nil

	case protocol.TypeProviderModelsResult:
		var p protocol.ProviderModelsResultPayload
		if err := env.Decode(&p); err != nil {
			return nil
		}
		if len(p.Models) == 0 {
			m.appendLine("供应商没有返回可用模型")
			return nil
		}
		m.modelPanel = newModelPanel(p.Models, m.sessionModel, p.Source)
		return nil

	case protocol.TypeTaskQueued:
		var p protocol.TaskPayload
		if err := env.Decode(&p); err == nil {
			// 回执的 status 恒为 queued（服务端先落库再标记 running）。
			// 排队信息只进队列；运行中任务的显示不能被它抢占。
			at := time.Time{}
			if p.CreatedAt != 0 {
				at = time.UnixMilli(p.CreatedAt)
			}
			m.enqueueQueued(domain.TaskID(p.ID), p.Prompt, at)
			if m.runTaskID == "" {
				m.taskID, m.taskStatus = p.ID, p.Status
			}
			m.appendLine(fmt.Sprintf("任务 %s 已提交，按会话内顺序执行（排队 %d）",
				shortID(p.ID), len(m.queue)))
		}
		return nil

	case protocol.TypeTaskCanceled:
		// 回执只表示「取消请求已被接受」：运行中的任务由服务端 agent 收尾，
		// 此刻可能仍是 running。终态以 task.status 事件为准，这里不能改本地状态。
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
	if env.Seq > 0 && env.Seq > m.lastSeq {
		m.lastSeq = env.Seq
	}
	// 运行中任务的事件才更新头部显示；排队任务的事件不能把 id 抢过去。
	if p.TaskID != "" && (m.runTaskID == "" || p.TaskID == m.runTaskID) {
		m.taskID = p.TaskID
	}
	if p.Kind == string(domain.EventApprovalRequested) {
		// 面板互斥：审批优先，否则用户会一边审 diff 一边管队列。
		m.queuePanel = nil
	}
	switch p.Kind {
	case string(domain.EventSessionCreated):
		// 同仓警告只在创建时出现：写路径有 CAS 保护，命令类工具则可能互踩。
		var s domain.SessionCreatedPayload
		if err := json.Unmarshal(p.Payload, &s); err == nil && len(s.SharedWorkspace) > 0 {
			m.appendLine(fmt.Sprintf("⚠ 同一仓库还有会话 %s 在运行（写入有 CAS 保护，命令可能互踩）",
				strings.Join(s.SharedWorkspace, ", ")))
		}
	case string(domain.EventSessionUpdated):
		var s domain.SessionUpdatedPayload
		if err := json.Unmarshal(p.Payload, &s); err == nil {
			m.sessionProvider, m.sessionModel = s.Provider, s.Model
			m.appendLine("模型已切换：" + modelLabel(s.Provider, s.Model))
		}
	case string(domain.EventTaskQueued):
		// 提问的唯一来源：实时提交与全量重放都靠这条事件重建用户可见的对话行。
		var q domain.TaskQueuedPayload
		if err := json.Unmarshal(p.Payload, &q); err == nil {
			m.enqueueQueued(domain.TaskID(p.TaskID), strings.TrimSpace(q.Prompt), time.Now())
			if prompt := strings.TrimSpace(q.Prompt); prompt != "" {
				m.appendLine("› " + prompt)
			}
		}
	case string(domain.EventTaskStatus):
		var s domain.TaskStatusPayload
		if err := json.Unmarshal(p.Payload, &s); err == nil {
			tid := p.TaskID
			switch s.To {
			case domain.TaskRunning, domain.TaskWaitingApproval:
				// 只有真正在执行的才是 esc 的目标。
				if tid != "" {
					m.runTaskID = tid
					m.taskID = tid
					m.removeQueued(domain.TaskID(tid))
				}
				m.taskStatus = string(s.To)
			case domain.TaskCompleted, domain.TaskFailed, domain.TaskCanceled:
				if tid != "" {
					m.removeQueued(domain.TaskID(tid))
				}
				// 排队任务被取消时，运行中任务的显示与流式文本不能被清掉。
				if tid == m.runTaskID || m.runTaskID == "" {
					m.taskStatus = string(s.To)
					m.stream = ""
					if tid != "" && tid == m.runTaskID {
						m.runTaskID = ""
					}
				}
			}
			if s.To == domain.TaskFailed && s.Error != "" {
				m.appendLine("失败：" + s.Error)
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
	case string(domain.EventContextCompacted):
		var c domain.ContextCompactedPayload
		if err := json.Unmarshal(p.Payload, &c); err == nil {
			m.appendLine(fmt.Sprintf("⋯ 上下文已压缩：%d 条较早消息 → %d 字摘要", c.CoveredMessages, c.SummaryChars))
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
			// status 取自工具调用的终态：拒绝、取消、失败是三种结局，
			// 拒绝不是「出错」。旧日志只有 ok/error，仍以 IsError 兜底。
			status := "完成"
			switch {
			case t.Status == string(domain.ToolDenied):
				status = "已拒绝"
			case t.Status == string(domain.ToolCanceled):
				status = "已取消"
			case t.IsError:
				status = "出错"
			}
			line := fmt.Sprintf("  ↳ %s %s（%d ms）", t.Tool, status, t.DurationMS)
			if t.ExitCode != nil {
				line += fmt.Sprintf(" · 退出码 %d", *t.ExitCode)
			}
			if t.Truncated {
				line += " · 输出已截断"
			}
			if t.OutputPath != "" {
				line += " · 全文已落盘"
			}
			m.appendLine(line)
			for _, extra := range outputPreview(t) {
				m.appendLine(extra)
			}
		}
	case string(domain.EventApprovalRequested):
		var a domain.ApprovalRequestedPayload
		if err := json.Unmarshal(p.Payload, &a); err == nil {
			m.showApprovalRequest(a, true)
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
// showApprovalRequest 把一个 approval.requested 载荷变成审批面板状态。
// 实时事件与重连快照共用；announce=false 时不重复写历史提示行。
func (m *Model) showApprovalRequest(a domain.ApprovalRequestedPayload, announce bool) {
	if a.Kind == domain.ApprovalCommand {
		m.approval = &approvalView{
			id:        string(a.ApprovalID),
			kind:      string(a.Kind),
			command:   a.Command,
			cwd:       a.Cwd,
			backend:   a.Backend,
			timeoutMS: a.TimeoutMS,
			reason:    a.Reason,
		}
		if announce {
			m.appendLine("⚠ 待审批命令：" + compact(strings.Join(a.Command, " ")))
		}
		return
	}
	if a.Kind == domain.ApprovalExternal {
		m.approval = &approvalView{
			id:     string(a.ApprovalID),
			kind:   string(a.Kind),
			tool:   a.Tool,
			source: a.Source,
			args:   a.Args,
			reason: a.Reason,
		}
		if announce {
			m.appendLine(fmt.Sprintf("⚠ 待审批外部工具：%s（%s）", a.Tool, a.Source))
		}
		return
	}
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
	if announce {
		m.appendLine("⚠ 待审批：" + a.Path)
	}
}

func (m *Model) View() tea.View {
	var b strings.Builder
	fmt.Fprintf(&b, "jiaoag %s  %s\n", version.Version, m.status)
	fmt.Fprintf(&b, "任务 %s · 状态 %s · 第 %d 轮 · token %d", shortID(m.taskID), orDash(m.taskStatus), m.round, m.tokens)
	if n := len(m.queue); n > 0 {
		fmt.Fprintf(&b, " · 排队 %d", n)
	}
	if m.sessionModel != "" {
		fmt.Fprintf(&b, " · %s", modelLabel(m.sessionProvider, m.sessionModel))
	}
	b.WriteByte('\n')
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
	case m.queuePanel != nil:
		b.WriteByte('\n')
		b.WriteString(m.renderQueuePanel())
	case m.modelPanel != nil:
		b.WriteByte('\n')
		b.WriteString(m.renderModelPanel())
	case m.sessionID != "":
		b.WriteString("\n› ")
		b.WriteString(m.input)
		b.WriteString("▏\n")
	}
	if m.approval == nil && m.queuePanel == nil && m.modelPanel == nil {
		if m.local != nil {
			b.WriteString("[enter] 发送  [ctrl+x] 终止本机命令  [esc] 取消/退出  [ctrl+q] 队列  [ctrl+r] 重连  [ctrl+c] 退出\n")
		} else {
			b.WriteString("[enter] 发送  [esc] 取消/退出  [ctrl+q] 队列  [ctrl+r] 重连  [/models] 模型  [!] 本机命令  [ctrl+c] 退出\n")
		}
	}
	return tea.NewView(b.String())
}

// resetView 清空本地会话视图，只保留 sessionID。
//
// 全量重放前先清空：历史、流式文本残余、待审批面板都从事件与快照重建。
func (m *Model) resetView() {
	m.taskID, m.taskStatus, m.stream = "", "", ""
	m.lastSeq = 0
	m.runTaskID = ""
	m.queue = nil
	m.queuePanel = nil
	m.modelPanel = nil
	m.round, m.tokens = 0, 0
	m.localLines, m.localOmitted = 0, 0
	m.dialog = nil
	m.approval = nil
}

// cancelable 报告本地是否认为有任务未结束（含排队）。esc 不依赖它：
// 取消只作用于 runTaskID，避免误取消排队任务。
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

// renderApprovalPanel 按审批类型渲染 diff 面板或命令面板。
func (m *Model) renderApprovalPanel() string {
	v := m.approval
	if v.kind == string(domain.ApprovalCommand) {
		return m.renderCommandPanel()
	}
	if v.kind == string(domain.ApprovalExternal) {
		return m.renderExternalPanel()
	}
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

// renderCommandPanel 逐 token 展示 argv，并写明 cwd / 后端 / 超时：用户批的
// 必须是「实际要跑的东西」，而不是一句「要不要执行命令」。
func (m *Model) renderCommandPanel() string {
	v := m.approval
	var b strings.Builder
	b.WriteString("⌘ 待审批命令\n")
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	// 长 token（上限 4 KB）必须折行：审批面板是用户判断「这串到底是什么」的
	// 唯一依据，一行冲出去看不见的部分等于没展示。
	for i, token := range v.command {
		prefix := fmt.Sprintf("  argv[%d]  ", i)
		width := panelWidth - len(prefix)
		if width < 8 {
			width = 8
		}
		chunks := wrapText(token, width)
		b.WriteString(prefix + chunks[0] + "\n")
		indent := strings.Repeat(" ", len(prefix))
		for _, chunk := range chunks[1:] {
			b.WriteString(indent + chunk + "\n")
		}
	}
	fmt.Fprintf(&b, "  cwd       %s\n", wrapOne(orDash(v.cwd), panelWidth-12))
	fmt.Fprintf(&b, "  backend   %s\n", orDash(v.backend))
	if v.timeoutMS > 0 {
		fmt.Fprintf(&b, "  timeout   %s\n", (time.Duration(v.timeoutMS) * time.Millisecond).String())
	}
	if v.reason != "" {
		fmt.Fprintf(&b, "  判定      %s\n", v.reason)
	}
	b.WriteString(strings.Repeat("─", 72))
	b.WriteByte('\n')
	b.WriteString("[y] 批准一次  [a] 本会话内都批准  [n] 拒绝\n")
	b.WriteString("[ctrl+r] 重连  [ctrl+c] 退出\n")
	return b.String()
}

// renderExternalPanel 展示一次外部工具调用。用户看不到 argv/diff，能判断的
// 只有「哪个来源的哪个工具、要带什么参数」——这三项必须完整可读。
func (m *Model) renderExternalPanel() string {
	v := m.approval
	var b strings.Builder
	b.WriteString("⇄ 待审批外部工具\n")
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  tool      %s\n", orDash(v.tool))
	fmt.Fprintf(&b, "  source    %s\n", orDash(v.source))
	if v.args != "" {
		chunks := wrapText(v.args, panelWidth-12)
		b.WriteString("  args      " + chunks[0] + "\n")
		for _, chunk := range chunks[1:] {
			b.WriteString("            " + chunk + "\n")
		}
	}
	if v.reason != "" {
		fmt.Fprintf(&b, "  判定      %s\n", wrapOne(v.reason, panelWidth-12))
	}
	b.WriteString(strings.Repeat("─", 72))
	b.WriteByte('\n')
	b.WriteString("[y] 批准一次  [a] 本会话内都批准  [n] 拒绝\n")
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

// wrapText 按显示宽度（rune 数）折行，不切断多字节字符。
func wrapText(s string, width int) []string {
	if s == "" {
		return []string{""}
	}
	if width <= 0 {
		return []string{s}
	}
	runes := []rune(s)
	out := make([]string, 0, len(runes)/width+1)
	for len(runes) > width {
		out = append(out, string(runes[:width]))
		runes = runes[width:]
	}
	return append(out, string(runes))
}

// wrapOne 把单行字段的首行折出来：面板其余部分靠 wrapText，这里只用于
// cwd 这类可能很长的单值字段。
func wrapOne(s string, width int) string {
	chunks := wrapText(s, width)
	if len(chunks) == 1 {
		return chunks[0]
	}
	return chunks[0] + "…"
}

// outputPreview 把命令输出折成界面上的几行。
//
// 摘要行只有退出码与耗时；「超时被终止」和编译/测试的失败诊断都只写在正文里。
// 不预览正文时，用户看到的就是一句「出错 · 退出码 1」，等于什么都没说。
func outputPreview(t domain.ToolResultPayload) []string {
	// 只有命令类工具会带退出码：读、搜类工具的输出不值得占对话区。
	if t.ExitCode == nil || strings.TrimSpace(t.Output) == "" {
		return nil
	}
	raw := strings.Split(strings.TrimRight(t.Output, "\n"), "\n")
	width := panelWidth - len([]rune(outputIndent))

	var out []string
	for i, line := range raw {
		chunks := wrapText(line, width)
		if len(out)+len(chunks) > maxOutputPreviewLines {
			rest := len(raw) - i
			out = append(out, fmt.Sprintf("  │ …（还有 %d 行%s）", rest, outputTailHint(t)))
			return out
		}
		for _, chunk := range chunks {
			out = append(out, outputIndent+chunk)
		}
	}
	return out
}

func outputTailHint(t domain.ToolResultPayload) string {
	if t.OutputPath != "" {
		return "，全文见输出日志"
	}
	return ""
}

func (m *Model) close() {
	if m.local != nil {
		// 退出客户端不该把 !命令 的进程组留在后台。
		m.local.Stop()
		m.local = nil
	}
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
