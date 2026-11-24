// Package tui 是 jiaoag 的终端客户端。
//
// M3 起它做三件事：自动建会话并订阅事件、把用户输入提交成任务、把流式
// 增量渲染成对话。渲染函数都是纯函数，便于在无终端环境里断言。
package tui

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	// ContinueLast 在没有指定 SessionID 时，接着本目录下最近更新的那个会话
	// 继续，而不是新开一个；同目录没有历史会话时退回新建。
	ContinueLast bool
	// InsecureTLS 跳过服务端证书校验，只用于自签证书的本机/内网部署。
	InsecureTLS bool
	// Roots 是要挂到新会话上的只读附加根（ADR-0036）。
	Roots []protocol.SessionRootPayload
	// Inline 关掉全屏：默认占满窗口、内容超出后自己滚（alt screen）；
	// 内联模式保留终端 scrollback，但状态栏会被历史推着走。
	Inline bool
	// Mouse 打开鼠标滚轮滚动（会占用终端原生选中，需要 shift 才复制）。
	Mouse bool
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
	// resuming 表示已经发出 session.list，正在等「继续最近会话」的候选列表。
	// 只有 -c 首次连接会置位，拿到结果就清掉，避免误吃后续的列表回包。
	resuming bool
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
	modelPanel *modelPanelView
	// skillPanel 非空表示 /skill 的技能选择面板打开。
	skillPanel *skillPanelView
	// mcpPanel 非空表示 /mcp 的 MCP server 面板打开（只读）。
	mcpPanel *mcpPanelView
	// forcedSkills 是用户点名、要随下一条任务提交的技能；提交后清空。
	forcedSkills []string
	// forcedModel 是 /model 选定的、只用于下一条任务的模型；提交后清空。
	forcedModel string
	// modelsForTask 标记正在等回包的模型列表用途：/model 只覆盖下一条任务。
	modelsForTask   bool
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

	// width/height 是当前终端尺寸（WindowSizeMsg 更新）。宽度决定折行，
	// 高度只在全屏时用来裁剪内容区：内联模式保留 scrollback，不该裁。
	width int
	// scroll 是全屏模式下从底部往回翻的行数；unseen 是翻页期间新增的行数。
	scroll int
	unseen int
	// mouse 决定是否向终端申请鼠标事件（只为滚轮）。
	mouse  bool
	height int
	// help 打开键位面板；fullscreen 决定是否用 alternate screen（状态栏
	// 才会真正钉在终端底行）。两者都只是本地视图状态，不落库。
	help       bool
	fullscreen bool
	styler     styler

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
	// 逐块选择：hunks 只在 diff 有 2 块及以上时才填。selectMode 打开后
	// 面板换成块列表，hunkSel 记录勾选状态，hunkCursor 是当前块。
	hunks      []patch.Hunk
	selectMode bool
	hunkSel    []bool
	hunkCursor int
	// collapsed 表示卡片被 esc 收起：审批仍然待处理，只是不占屏。
	// 状态栏照旧显示「等待审批」，再按 esc 展开。
	collapsed bool
	// focus 是三个按钮里当前获得焦点的那一个（0 批准一次 / 1 本会话 / 2 拒绝）。
	// tab 与 ←→ 改变它，enter 执行；y/a/n 仍然直达，不需要先移焦点。
	focus int
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
		opts:       opts,
		status:     "连接中…",
		maxLines:   500,
		width:      100,
		height:     30,
		fullscreen: !opts.Inline,
		mouse:      opts.Mouse,
		styler:     newStyler(),
		sessionID:  opts.SessionID,
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

// insecureHTTPClient 返回跳过证书校验的客户端。只由 -insecure 显式开启，
// 用于自签证书的本机或内网部署；默认仍走系统信任链。
func insecureHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
}

// Dial 建立连接并完成握手。
func Dial(ctx context.Context, opts Options) (*websocket.Conn, protocol.HelloPayload, error) {
	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + opts.Token}},
	}
	if opts.InsecureTLS {
		dialOpts.HTTPClient = insecureHTTPClient()
	}
	conn, _, err := websocket.Dial(ctx, opts.URL, dialOpts)
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

// taskSubmitPayload 组装一次提交：点名的技能只随这一条任务走，用完即清。
func (m *Model) taskSubmitPayload(text string) protocol.TaskSubmitPayload {
	p := protocol.TaskSubmitPayload{
		SessionID: m.sessionID,
		Text:      text,
		Skills:    m.forcedSkills,
	}
	if m.forcedModel != "" {
		p.Provider = m.sessionProvider
		p.Model = m.forcedModel
	}
	return p
}

// armTaskModel 把模型绑定到下一条任务。
//
// 刻意不改会话：用户往往只是想用便宜模型做一次检索，改会话会让后面的任务
// 都在不知情的情况下换了模型。
func (m *Model) armTaskModel(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		m.appendLine("模型名不能为空")
		return
	}
	m.forcedModel = model
	m.appendLine("下一条任务使用模型：" + modelLabel(m.sessionProvider, model) + "（提交后恢复会话默认）")
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

// latestSessionFor 从 session.list 回包（服务端已按 updated_at 倒序）里挑
// 第一个工作区与当前目录相同的会话。-c 只在本目录内继续，否则会把另一个
// 项目的会话拉进来，任务随后就跑在那个工作区里。
func latestSessionFor(sessions []protocol.SessionPayload, workspace string) (protocol.SessionPayload, bool) {
	want := canonicalPath(workspace)
	for _, sess := range sessions {
		if canonicalPath(sess.Workspace) == want {
			return sess, true
		}
	}
	return protocol.SessionPayload{}, false
}

// canonicalPath 展开符号链接：macOS 上 /var 与 /private/var 是同一处，
// 不归一化会让同一个目录看起来不同。
func canonicalPath(p string) string {
	clean := filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	return clean
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
		if m.opts.ContinueLast {
			// 先问服务端本目录下有没有历史会话；有就订阅它，没有才新建。
			m.resuming = true
			m.status = "正在查找最近的会话…"
			return m, tea.Batch(
				waitForEnvelope(m.conn),
				sendCmd(m.conn, protocol.TypeSessionList, nil),
			)
		}
		return m, tea.Batch(
			waitForEnvelope(m.conn),
			sendCmd(m.conn, protocol.TypeSessionCreate, protocol.SessionCreatePayload{
				Workspace: m.opts.Workspace,
				Roots:     m.opts.Roots,
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

	case tea.MouseWheelMsg:
		if !m.fullscreen {
			return m, nil
		}
		n := 3
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scroll += n
		case tea.MouseWheelDown:
			m.scroll -= n
			if m.scroll < 0 {
				m.scroll = 0
			}
		default:
			return m, nil
		}
		if m.scroll == 0 {
			m.unseen = 0
		}
		return m, nil
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
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
	// ctrl+f 切全屏：全屏时状态栏才真正钉在终端底行，代价是退出后没有
	// scrollback。属于显示偏好，任何面板打开时都可以切。
	if key == "ctrl+f" {
		m.fullscreen = !m.fullscreen
		return m, nil
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
	if m.skillPanel != nil {
		return m.handleSkillKey(key)
	}
	if m.mcpPanel != nil {
		return m.handleMCPKey(key)
	}
	if m.help {
		// 帮助是覆盖层：除了 ctrl+c / ctrl+f 与关闭键，其余按键都不该穿透。
		switch key {
		case "?", "esc":
			m.help = false
		}
		return m, nil
	}
	switch key {
	case "pgup", "pgdown", "ctrl+u", "ctrl+d", "home", "end":
		// 只有全屏才需要自己滚：内联模式交给终端 scrollback。
		if !m.fullscreen {
			return m, nil
		}
		page := m.height - 8
		if page < 5 {
			page = 5
		}
		switch key {
		case "pgup":
			m.scroll += page
		case "pgdown":
			m.scroll -= page
		case "ctrl+u":
			m.scroll += page / 2
		case "ctrl+d":
			m.scroll -= page / 2
		case "home":
			// 回到最早的一屏：scroll 会被 View 夹到内容长度。
			m.scroll = len(m.dialog)
		case "end":
			m.scroll = 0
			m.unseen = 0
		}
		if m.scroll < 0 {
			m.scroll = 0
		}
		if m.scroll == 0 {
			m.unseen = 0
		}
		return m, nil
	case "?":
		m.help = true
		return m, nil
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
			m.modelsForTask = false
			m.appendLine("正在获取模型列表…")
			return m, sendCmd(m.conn, protocol.TypeProviderModels, protocol.ProviderModelsPayload{
				SessionID: m.sessionID,
			})
		}
		// /model 只覆盖下一条任务：带参数直接绑定，否则开面板选。
		if text == "/model" || strings.HasPrefix(text, "/model ") {
			m.input = ""
			if name := strings.TrimSpace(strings.TrimPrefix(text, "/model")); name != "" {
				m.armTaskModel(name)
				return m, nil
			}
			m.modelsForTask = true
			m.appendLine("正在获取模型列表…")
			return m, sendCmd(m.conn, protocol.TypeProviderModels, protocol.ProviderModelsPayload{
				SessionID: m.sessionID,
			})
		}
		// /skill 选择下一条任务要加载的技能：带参数就直接点名，否则开面板。
		if text == "/skill" || strings.HasPrefix(text, "/skill ") {
			m.input = ""
			if name := strings.TrimSpace(strings.TrimPrefix(text, "/skill")); name != "" {
				m.armSkill(name)
				return m, nil
			}
			m.appendLine("正在获取技能列表…")
			return m, sendCmd(m.conn, protocol.TypeSkillList, protocol.SkillListPayload{
				SessionID: m.sessionID,
			})
		}
		// /mcp 只读展示当前工作区的 MCP server：起了哪些、各几个工具、什么信任级别。
		if text == "/mcp" {
			m.input = ""
			m.appendLine("正在获取 MCP server 列表…")
			return m, sendCmd(m.conn, protocol.TypeMCPList, protocol.MCPListPayload{
				SessionID: m.sessionID,
			})
		}
		// /trust 确认当前工作区的项目级扩展（AGENTS.md、.jiaoag/、.agents/skills）。
		if text == "/roots" {
			m.input = ""
			if len(m.opts.Roots) == 0 {
				m.appendLine("没有附加根：启动时用 -root alias=path 挂载（只读）")
			} else {
				m.appendLine(fmt.Sprintf("附加根 %d 个（只读）：", len(m.opts.Roots)))
				for _, r := range m.opts.Roots {
					m.appendLine(fmt.Sprintf("  %s → %s", r.Alias, r.Path))
				}
			}
			return m, nil
		}
		if text == "/trust" {
			m.input = ""
			m.appendLine("正在确认项目级扩展…")
			return m, sendCmd(m.conn, protocol.TypeProjectTrust, protocol.ProjectActionPayload{
				SessionID: m.sessionID,
			})
		}
		if text == "/untrust" {
			m.input = ""
			m.appendLine("正在撤销项目级扩展的信任…")
			return m, sendCmd(m.conn, protocol.TypeProjectRevoke, protocol.ProjectActionPayload{
				SessionID: m.sessionID,
			})
		}
		// 运行中也可以提交：服务端会把它排到队尾，会话内仍是串行执行。
		//
		// 提问不回显在这里：它由 task.queued 事件统一渲染，否则断线重连后的
		// 全量重放会缺用户原话，而实时提交又会打两遍。
		payload := m.taskSubmitPayload(text)
		m.input = ""
		m.forcedSkills = nil
		m.forcedModel = ""
		return m, sendCmd(m.conn, protocol.TypeTaskSubmit, payload)
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

	case protocol.TypeSessionListResult:
		if !m.resuming {
			return nil
		}
		m.resuming = false
		var p protocol.SessionListResultPayload
		if err := env.Decode(&p); err != nil {
			return nil
		}
		target, ok := latestSessionFor(p.Sessions, m.opts.Workspace)
		if !ok {
			m.appendLine("这个目录还没有历史会话，已新建")
			return sendCmd(m.conn, protocol.TypeSessionCreate, protocol.SessionCreatePayload{
				Workspace: m.opts.Workspace,
				Roots:     m.opts.Roots,
				Provider:  m.opts.Provider,
				Model:     m.opts.Model,
			})
		}
		m.resetView()
		m.sessionID = target.ID
		m.sessionProvider, m.sessionModel = target.Provider, target.Model
		m.status = fmt.Sprintf("会话 %s · %s", target.ID, target.Workspace)
		m.appendLine("继续会话 " + target.ID)
		if target.Title != "" {
			m.appendLine("标题：" + target.Title)
		}
		if target.Model != "" {
			m.appendLine("模型：" + modelLabel(target.Provider, target.Model))
		}
		return sendCmd(m.conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
			SessionID: m.sessionID,
			FromSeq:   0,
		})

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
			m.noteProject(p.Project)
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

	case protocol.TypeProjectTrusted:
		var p protocol.ProjectStatusPayload
		if err := env.Decode(&p); err != nil {
			return nil
		}
		if p.Project.Trusted {
			m.appendLine("✔ 已信任项目级扩展：" + p.Project.Summary + "（下一条任务生效）")
			for _, prompt := range p.Project.Prompts {
				m.appendLine("  · 提示 " + prompt)
			}
			for _, srv := range p.Project.Servers {
				m.appendLine("  · MCP " + srv)
			}
		} else {
			m.appendLine("已撤销项目级扩展信任，下一条任务起不再加载")
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
		current := m.sessionModel
		if m.modelsForTask && m.forcedModel != "" {
			current = m.forcedModel
		}
		m.modelPanel = newModelPanelFor(p.Models, current, p.Source, m.modelsForTask)
		m.modelsForTask = false
		return nil

	case protocol.TypeSkillListResult:
		var p protocol.SkillListResultPayload
		if err := env.Decode(&p); err != nil {
			return nil
		}
		if len(p.Skills) == 0 {
			m.appendLine("技能库为空：检查 agent.skills 与 skills_dirs")
			return nil
		}
		armed := ""
		if len(m.forcedSkills) > 0 {
			armed = m.forcedSkills[0]
		}
		m.skillPanel = newSkillPanel(p.Skills, armed)
		return nil

	case protocol.TypeMCPListResult:
		var p protocol.MCPListResultPayload
		if err := env.Decode(&p); err != nil {
			return nil
		}
		if len(p.Servers) == 0 {
			m.appendLine("没有配置 MCP server：检查 mcp.servers 或工作区的 .jiaoag/mcp.yaml")
			return nil
		}
		m.mcpPanel = newMCPPanel(p.Servers)
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
			if len(q.Skills) > 0 {
				m.appendLine("⋯ 加载技能：" + strings.Join(q.Skills, ", "))
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
			skillLoaded := t.Tool == "skill" && !t.IsError
			if skillLoaded {
				line += " · 技能正文已载入"
			}
			m.appendLine(line)
			// 技能正文是给模型的操作手册，逐行铺在对话里只会淹没上下文。
			if !skillLoaded {
				for _, extra := range outputPreview(t) {
					m.appendLine(extra)
				}
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
	view := &approvalView{
		id:        string(a.ApprovalID),
		path:      a.Path,
		diff:      a.Diff,
		truncated: a.Truncated,
		added:     added,
		removed:   removed,
	}
	// 只有一块时逐块选择没有意义，直接走全量视图。
	if hs, err := patch.ParseHunks(a.Diff); err == nil && len(hs) > 1 {
		view.hunks = hs
		view.hunkSel = make([]bool, len(hs))
		for i := range view.hunkSel {
			view.hunkSel[i] = true
		}
	}
	m.approval = view
	// 面板占屏，提示行要短：详情在下面几行。
	if announce {
		m.appendLine("⚠ 待审批：" + a.Path)
	}
}

func (m *Model) View() tea.View {
	width := m.width
	if width <= 0 {
		width = 100
	}
	height := m.height
	if height <= 0 {
		height = 30
	}

	// 一、内容：历史 + 正在流式的段落。样式只在渲染时施加，
	// m.dialog 里始终是纯文本，回放与测试都不受颜色影响。
	content := make([]string, 0, len(m.dialog)+1)
	for _, line := range m.dialog {
		// 一条历史里可能夹着换行（工具输出、面板摘要）。先拆开，否则下面
		// 按「条目数」算高度会低估实际占用的行数，底部区就被推出屏幕。
		for _, part := range strings.Split(line, "\n") {
			content = append(content, m.styler.stylizeLine(part))
		}
	}
	if m.stream != "" {
		// 流式文本里带换行是常态：按行拆开，首行带角色前缀，续行缩进两格。
		// 只有这样，光标（▏）才会落在真正最后一行的末尾。
		parts := strings.Split(m.stream, "\n")
		for i, part := range parts {
			var line string
			if i == 0 {
				line = m.styler.paint(styleStrong, "jiaoag") + m.styler.paint(styleMuted, ": ") + part
			} else {
				line = "  " + part
			}
			if i == len(parts)-1 && m.cancelable() {
				line += m.styler.paint(styleAccent, "▏")
			}
			content = append(content, line)
		}
	}

	// 二、模态区：审批 / 各面板 / ? 帮助。它们占住输入位置，
	// 因为此刻按键语义已经不属于「输入文本」。
	modal := true
	var modalText string
	switch {
	case m.approval != nil && !m.approval.collapsed:
		modalText = m.renderApprovalPanel()
	case m.queuePanel != nil:
		modalText = m.renderQueuePanel()
	case m.modelPanel != nil:
		modalText = m.renderModelPanel()
	case m.skillPanel != nil:
		modalText = m.renderSkillPanel()
	case m.mcpPanel != nil:
		modalText = m.renderMCPPanel()
	case m.help:
		modalText = m.renderHelpPanel()
	default:
		modal = false
	}
	var modalLines []string
	if modalText != "" {
		modalLines = strings.Split(strings.TrimRight(modalText, "\n"), "\n")
	}
	// 所有行都先折成视觉行：面板底栏、输入行也可能比屏幕宽，终端一折行，
	// 整帧就比终端高一行，alt screen 会因此产生滚动backlog——用户往上滚
	// 就会看到旧帧。折行后再统一按视觉行数排版。
	wrapAll := func(lines []string) []string {
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			// 一条内容里可能还夹着换行——最典型的是流式输出：模型一个 delta
			// 里带 \n 很常见，拼进 m.stream 之后就是一行里藏着断行。不先拆开，
			// 终端会按真实换行渲染出更多行，帧高就比预期多一些，底部区被顶走。
			for _, seg := range strings.Split(line, "\n") {
				out = append(out, wrapStyled(seg, width)...)
			}
		}
		return out
	}

	// 三、底部固定区：分隔线 + 输入行 + 状态栏。
	//
	// 输入行紧贴状态栏（编辑器与 IM 客户端的惯例）：最后一行永远是状态，
	// 输入就在它上面一行，中间不再夹分隔线。键位提示并进状态栏右侧，
	// 面板打开时由面板自己的底栏负责，不重复占一行。
	// 底部区固定三行：分隔线、输入行、状态栏。输入行即使没有会话或面板
	// 打开时也照样占位（换成灰色说明），这样底部高度不随状态变化，内容区
	// 的可见行数也就稳定。
	dock := make([]string, 0, dockRows)
	dock = append(dock, m.styler.paint(styleMuted, strings.Repeat("-", width)))
	dock = append(dock, m.renderInputRow(width, modal))
	dock = append(dock, strings.Split(strings.TrimRight(m.renderStatusBar(width, !modal), "\n"), "\n")...)
	dock = wrapAll(dock)
	modalLines = wrapAll(modalLines)

	// 四、全屏时按终端高度裁剪内容区：状态栏与输入必须真的贴底，
	// 而不是被一堆历史行推到屏幕外。内联模式不裁——那正是它保留
	// scrollback 的意义。
	if m.fullscreen {
		// 模态区前面会补一个空行，算预算时要一起扣掉，否则底部会整体上移一行。
		extra := 0
		if len(modalLines) > 0 {
			extra = 1
		}
		avail := height - len(dock) - len(modalLines) - extra
		if avail < 1 {
			avail = 1
		}
		// 终端会按显示宽度折行：一行 200 字在实际屏幕上是两三行。只按条目数
		// 裁剪会低估高度，底部区就被挤出屏幕——先把内容摊成视觉行，再按行开窗。
		rows := wrapAll(content)
		maxScroll := len(rows) - avail
		if maxScroll < 0 {
			maxScroll = 0
		}
		if m.scroll > maxScroll {
			m.scroll = maxScroll
		}
		end := len(rows) - m.scroll
		start := end - avail
		if start < 0 {
			start = 0
		}
		rows = rows[start:end]
		if start > 0 {
			rows[0] = m.styler.paint(styleMuted, fmt.Sprintf("… 上方还有 %d 行（pgup/pgdn 翻）", start))
		}
		for len(rows) < avail {
			rows = append(rows, "")
		}
		content = rows
	}

	// 最终组装。极端窄屏或超长输入会让底部区自己就超过一屏：这时只能
	// 从顶部裁，保证状态栏永远在最后一行——它被挤掉就等于失去了唯一的
	// 状态读数。
	out := make([]string, 0, len(content)+len(modalLines)+len(dock)+1)
	out = append(out, content...)
	if len(modalLines) > 0 {
		out = append(out, "")
		out = append(out, modalLines...)
	}
	out = append(out, dock...)
	if m.fullscreen && height > 0 && len(out) > height {
		out = out[len(out)-height:]
	}

	// 全屏模式下末尾不能再跟一个换行：渲染器按整个屏幕画帧，多出的空行
	// 会让帧高变成 height+1，终端于是滚动一行，最后一行（状态栏）与它上面
	// 的输入框会在滚动的那一瞬间被顶出屏幕——表现为「闪一下不见了」。
	text := strings.Join(out, "\n")
	if !m.fullscreen {
		text += "\n"
	}

	view := tea.NewView(text)
	view.AltScreen = m.fullscreen
	if m.mouse && m.fullscreen {
		view.MouseMode = tea.MouseModeCellMotion
	}
	return view
}

// renderStatusBar 是底部状态栏：左边是「在哪、在干什么」，右边是「用的是什么模型、花了多少 token」。
//
// 放底部是终端应用的惯例（vim、tmux、htop）：视线从内容往下扫就能确认状态，
// 顶部第一行留给内容本身。
// dockRows 是底部固定区的高度：分隔线 + 输入行 + 状态栏。
const dockRows = 3

// renderInputRow 永远返回一行的输入行。没有会话、或面板打开时用灰色说明
// 占位，保证底部区高度恒定——布局不随输出或状态跳变。
func (m *Model) renderInputRow(width int, modal bool) string {
	s := m.styler
	prefix := s.paint(styleAccent, "› ")
	room := width - displayWidth("› ") - 1
	if room < 4 {
		room = 4
	}
	var text string
	switch {
	case modal:
		return s.paint(styleMuted, "› ") + s.paint(styleMuted, "面板打开中，esc 返回")
	case m.sessionID == "":
		return s.paint(styleMuted, "› ") + s.paint(styleMuted, "等待会话…")
	default:
		text = m.input
	}
	if displayWidth(text) > room {
		text = "…" + tailDisplay(text, room-1)
	}
	line := prefix + text + s.paint(styleAccent, "▏")
	if displayWidth(line) > width {
		line = truncateDisplay(line, width)
	}
	return line
}

func (m *Model) renderStatusBar(width int, withHints bool) string {
	s := m.styler
	// 左侧每项带一个“可丢弃”等级：放不下时先丢等级高的，而不是把模型名
	// 这类最关键的信息先砍掉。
	type segment struct {
		text string
		drop int
	}
	var left []segment
	var right []string

	conn := m.conn != nil
	if conn {
		left = append(left, segment{s.paint(styleOK, "●") + " " + s.paint(styleMuted, "已连接"), 0})
	} else {
		left = append(left, segment{s.paint(styleErr, "●") + " " + s.paint(styleMuted, "未连接"), 0})
	}
	if dir := m.abbrevWorkspace(); dir != "" {
		left = append(left, segment{s.paint(styleStrong, "⌂ "+dir), 1})
	}
	left = append(left, segment{m.statusChip(), 0})
	if m.taskID != "" {
		left = append(left, segment{s.paint(styleMuted, shortID(m.taskID)), 3})
	}
	if m.round > 0 {
		left = append(left, segment{s.paint(styleMuted, fmt.Sprintf("第 %d 轮", m.round)), 4})
	}
	if n := len(m.queue); n > 0 {
		left = append(left, segment{s.paint(styleWarn, fmt.Sprintf("排队 %d", n)), 2})
	}
	if m.forcedModel != "" {
		left = append(left, segment{s.paint(styleWarn, "下一条 "+m.forcedModel), 3})
	}
	if len(m.forcedSkills) > 0 {
		left = append(left, segment{s.paint(styleMuted, "技能 "+strings.Join(m.forcedSkills, ",")), 5})
	}
	// 翻页状态：只在上翻时出现。有新输出时给一个「↓ N 新行」，
	// 相当于聊天应用里那个回到最新的提示。
	if m.scroll > 0 {
		left = append(left, segment{s.paint(styleWarn, fmt.Sprintf("↕ 上翻 %d 行", m.scroll)), 2})
	}
	if m.unseen > 0 {
		left = append(left, segment{s.paint(styleAccent, fmt.Sprintf("↓ %d 新行 · end 回底", m.unseen)), 1})
	}

	if m.sessionModel != "" {
		right = append(right, s.paint(styleAccent, modelLabel(m.sessionProvider, m.sessionModel)))
	}
	if m.tokens > 0 {
		right = append(right, s.paint(styleMuted, humanTokens(m.tokens)+" tok"))
	}

	joinLeft := func(segs []segment) string {
		parts := make([]string, 0, len(segs))
		for _, seg := range segs {
			parts = append(parts, seg.text)
		}
		return strings.Join(parts, "  ")
	}
	// 右侧是「模型 + token」，都比左边那些计数更重要：先按等级丢左侧项去
	// 腾位置，实在放不下才逐项丢右侧。
	fullRight := strings.Join(right, "  ")
	for displayWidth(joinLeft(left))+displayWidth(fullRight)+2 > width && len(left) > 3 {
		worst := 1
		for i, seg := range left {
			if seg.drop > left[worst].drop {
				worst = i
			}
		}
		left = append(left[:worst], left[worst+1:]...)
	}
	var keptRight []string
	for i := len(right) - 1; i >= 0; i-- {
		candidate := append([]string{right[i]}, keptRight...)
		if displayWidth(joinLeft(left))+displayWidth(strings.Join(candidate, "  "))+2 > width {
			continue
		}
		keptRight = candidate
	}
	rightLine := strings.Join(keptRight, "  ")
	for displayWidth(joinLeft(left))+displayWidth(rightLine)+2 > width && len(left) > 3 {
		worst := 1
		for i, seg := range left {
			if seg.drop > left[worst].drop {
				worst = i
			}
		}
		left = append(left[:worst], left[worst+1:]...)
	}
	leftLine := joinLeft(left)
	if displayWidth(leftLine) > width {
		leftLine = truncateDisplay(leftLine, width)
	}
	gap := width - displayWidth(leftLine) - displayWidth(rightLine)
	if gap < 1 {
		gap = 1
	}
	line := leftLine + strings.Repeat(" ", gap) + rightLine
	// 键位提示填在左块与右块之间的空隙里，紧挨着右侧。它是最先被牺牲的
	// 信息：放不下就按项截断，一项都放不下就整段省略——绝不挤掉模型与用量。
	if withHints {
		if hint := m.fitHints(gap - 2); hint != "" {
			// 提示必须连同最小间隔一起塞得进去，否则宁可整段不要：状态栏
			// 多出一个字符就会被终端折成两行，整帧高度对不上，alt screen
			// 跟着滚，底部固定区就保不住了。
			if pad := gap - displayWidth(hint); pad >= 3 {
				line = leftLine + strings.Repeat(" ", pad) + hint + rightLine
			}
		}
	}
	if displayWidth(line) > width {
		line = truncateDisplay(line, width)
	}
	return line + "\n"
}

// statusChip 把任务状态翻成一个色块。状态词与事件里的英文值分开：
// 界面说中文，日志与审计说英文，两边都能查。
func (m *Model) statusChip() string {
	s := m.styler
	// 审批面板存在时状态就是「等裁决」：任务可能已被服务端标成
	// waiting_approval，但本地拿到事件之前也不该显示成运行中。
	if m.approval != nil {
		return s.paint(styleWarn, "[等待审批]")
	}
	switch m.taskStatus {
	case string(domain.TaskRunning):
		return s.paint(styleAccent, "[运行中]")
	case string(domain.TaskWaitingApproval):
		return s.paint(styleWarn, "[等待审批]")
	case string(domain.TaskQueued):
		return s.paint(styleWarn, "[排队中]")
	case string(domain.TaskCompleted):
		return s.paint(styleOK, "[已完成]")
	case string(domain.TaskFailed):
		return s.paint(styleErr, "[失败]")
	case string(domain.TaskCanceled):
		return s.paint(styleMuted, "[已取消]")
	}
	return s.paint(styleMuted, "[空闲]")
}

// fitHints 按项往给定宽度里塞键位提示：整项放不下就停，不切断某一项
// （半截的「enter 发」比没有更让人困惑）。
func (m *Model) fitHints(max int) string {
	if max <= 0 {
		return ""
	}
	sep := m.styler.paint(styleMuted, " · ")
	var kept []string
	for _, part := range m.hintParts(0) {
		candidate := part
		if len(kept) > 0 {
			candidate = strings.Join(kept, sep) + sep + part
		}
		if displayWidth(candidate) > max {
			break
		}
		kept = append(kept, part)
	}
	if len(kept) < 2 {
		return ""
	}
	return strings.Join(kept, sep)
}

// hintParts 是当前状态下的键位提示项，按重要性从高到低排列。
// 面板打开时显示面板的键，审批时显示裁决的键。
func (m *Model) hintParts(width int) []string {
	s := m.styler
	k := func(key, label string) string { return s.paint(styleAccent, key) + " " + label }

	var parts []string
	switch {
	case m.approval != nil && m.approval.collapsed:
		parts = []string{k("esc", "展开待审批"), k("y/a/n", "直选"), k("?", "键位")}
	case m.approval != nil:
		if m.approval.selectMode {
			parts = []string{k("space", "勾选"), k("→←/jk", "换块"), k("y", "应用选中"), k("esc", "返回")}
		} else {
			parts = []string{k("tab", "切换按钮"), k("enter", "确认"), k("y/a/n", "直选"), k("v", "逐块")}
		}
	case m.queuePanel != nil:
		parts = []string{k("↑↓", "选择"), k("d", "取消任务"), k("esc", "关闭")}
	case m.modelPanel != nil:
		parts = []string{k("↑↓", "选择"), k("enter", "应用"), k("esc", "关闭")}
	case m.skillPanel != nil:
		parts = []string{k("↑↓", "选择"), k("enter", "指定"), k("x", "清除"), k("esc", "关闭")}
	case m.mcpPanel != nil:
		parts = []string{k("↑↓", "选择"), k("esc", "关闭")}
	case m.help:
		parts = []string{k("esc", "关闭")}
	case m.local != nil:
		parts = []string{k("ctrl+x", "终止本机命令"), k("?", "键位"), k("ctrl+c", "退出")}
	default:
		parts = []string{k("enter", "发送"), k("esc", "取消"), k("?", "键位"), k("ctrl+q", "队列")}
		if width >= 90 {
			parts = append(parts, k("/models", "会话模型"), k("/model", "单次模型"), k("/skill", "技能"))
		}
	}
	return parts
}

// renderHelpPanel 是 ? 面板：版本、会话、完整键位、当前策略都收在这里，
// 不占常驻行。
func (m *Model) renderHelpPanel() string {
	s := m.styler
	var b strings.Builder
	b.WriteString(s.paint(styleAccent, "⌨ 键位与状态") + s.paint(styleMuted, fmt.Sprintf("  jiaoag %s", version.Version)) + "\n")
	b.WriteString(s.paint(styleMuted, strings.Repeat("─", panelWidth)) + "\n")
	rows := [][2]string{
		{"enter", "提交任务 / 确认面板动作"},
		{"esc", "取消运行中的任务 / 关闭面板 / 退出"},
		{"?", "打开或关闭这个面板"},
		{"ctrl+q", "队列面板"},
		{"ctrl+r", "重连"},
		{"ctrl+f", "切换全屏 / 内联（默认全屏，状态栏钉底）"},
		{"pgup/pgdn", "全屏下翻页"},
		{"ctrl+u/d", "全屏下半页翻"},
		{"home/end", "跳到最早 / 回到最新"},
		{"ctrl+x", "终止正在跑的 !本机命令"},
		{"/models", "切换会话模型"},
		{"/model", "只给下一条任务指定模型"},
		{"/skill", "指定下一条任务加载的技能"},
		{"/mcp", "MCP server 与工具数"},
		{"/roots", "会话挂载的附加根"},
		{"!cmd", "在本机执行命令（不进模型、不审计）"},
		{"v", "审批时逐块选择"},
		{"tab", "审批时切换三个按钮"},
		{"ctrl+c", "退出"},
	}
	for _, row := range rows {
		fmt.Fprintf(&b, "%s%s\n", padDisplay(s.paint(styleAccent, row[0]), 10), s.paint(stylePlain, row[1]))
	}
	b.WriteString(s.paint(styleMuted, strings.Repeat("─", panelWidth)) + "\n")
	info := []string{}
	if m.sessionID != "" {
		info = append(info, "会话 "+m.sessionID)
	}
	if m.opts.Workspace != "" {
		info = append(info, "工作区 "+m.opts.Workspace)
	}
	for _, r := range m.opts.Roots {
		info = append(info, "附加根 "+r.Alias+"="+r.Path)
	}
	for _, line := range info {
		b.WriteString(s.paint(styleMuted, line) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// abbrevWorkspace 把工作区路径缩成 ~/... 形式。状态栏宽度有限，
// 而仓库名在最右边——保留尾部比保留开头更有用。
func (m *Model) abbrevWorkspace() string {
	path := m.opts.Workspace
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	if displayWidth(path) <= 28 {
		return path
	}
	return truncateMiddle(path, 28)
}

// humanTokens 把 token 数缩成 1.2k 形式：状态栏只需要量级，精确值在审计里。
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// truncateMiddle 保留首尾、砍中间：路径里最重要的是开头（~ 还是绝对路径）
// 与结尾（仓库名）。
func truncateMiddle(s string, width int) string {
	if displayWidth(s) <= width || width < 8 {
		return truncateDisplay(s, width)
	}
	left := width / 2
	right := width - left - 1
	return headDisplay(s, left) + "…" + tailDisplay(s, right)
}

// headDisplay 按显示宽度取头部（不加省略号）。
func headDisplay(s string, width int) string {
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runeWidth(r)
		if used+w > width {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}

// tailDisplay 按显示宽度取尾部。
func tailDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	used := 0
	i := len(runes)
	for i > 0 {
		w := runeWidth(runes[i-1])
		if used+w > width {
			break
		}
		used += w
		i--
	}
	return string(runes[i:])
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
	m.skillPanel = nil
	m.mcpPanel = nil
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
	if v.selectMode {
		return m.handleHunkKey(key)
	}
	total := len(strings.Split(strings.TrimRight(v.diff, "\n"), "\n"))
	switch key {
	case "esc":
		// 收起而非关闭：审批还在，只是让出屏幕；再按 esc 展开。
		v.collapsed = !v.collapsed
	case "tab", "right", "l":
		v.focus = (v.focus + 1) % 3
	case "shift+tab", "left", "h":
		v.focus = (v.focus + 2) % 3
	case "enter":
		return m.resolveApproval(approvalDecision(v.focus))
	case "v":
		// 逐块审批：只对多块 diff 开放，单块时全量面板已经够清楚。
		if len(v.hunks) > 1 {
			v.selectMode = true
			v.hunkCursor = 0
		}
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
		v.offset -= m.diffRows() - 2
		if v.offset < 0 {
			v.offset = 0
		}
	case "pgdown":
		v.offset += m.diffRows() - 2
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
	payload := m.approvalResolvePayload(v, d)
	if len(payload.Hunks) > 0 && d == domain.DecisionAllowOnce {
		m.appendLine(fmt.Sprintf("⋯ 只应用第 %s 块，其余块保持原内容", joinHunkNumbers(payload.Hunks)))
	}
	return m, sendCmd(m.conn, protocol.TypeApprovalResolve, payload)
}

// approvalResolvePayload 把面板状态翻成裁决帧。抽出来是为了能在没有连接时断言
// 「部分块」到底发了什么——resolveApproval 本身依赖真实连接。
func (m *Model) approvalResolvePayload(v *approvalView, d domain.Decision) protocol.ApprovalResolvePayload {
	payload := protocol.ApprovalResolvePayload{
		SessionID:  m.sessionID,
		ApprovalID: v.id,
		Decision:   string(d),
	}
	if hunks := partialHunks(v); len(hunks) > 0 {
		payload.Hunks = hunks
	}
	return payload
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

// renderPanelHint 把一行键位提示按面板宽度折行输出（带换行）。
//
// 提示里混了中文与 ASCII：必须按显示宽度折，不能按 rune 数。
func (m *Model) renderPanelHint(text string) string {
	var b strings.Builder
	for i, line := range wrapDisplay(text, panelWidth) {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.styler.paint(styleMuted, line))
	}
	return b.String()
}

// diffRows 是审批面板一次展示多少行 diff：默认 18，终端矮时跟着缩，
// 否则卡片会顶掉输入行与状态栏。
func (m *Model) diffRows() int {
	h := m.height
	if h <= 0 {
		return diffPanelHeight
	}
	rows := h - 14
	if rows < 6 {
		rows = 6
	}
	if rows > diffPanelHeight {
		rows = diffPanelHeight
	}
	return rows
}

// approvalDecision 把按钮焦点翻成裁决。抽成纯函数是为了让「第几个按钮」
// 与「发什么决定」的映射能被直接测试，而不必真的连上服务端。
func approvalDecision(focus int) domain.Decision {
	switch focus {
	case 1:
		return domain.DecisionAllowSession
	case 2:
		return domain.DecisionDeny
	}
	return domain.DecisionAllowOnce
}

// renderApprovalButtons 画出三个按钮，焦点用 ▸ ◂ 包住。
//
// 为什么不是单选高亮块：终端里背景色不可靠（配色方案、无背景终端、屏幕
// 阅读器），用可见的括选符号能把焦点表达在所有降级环境里。y/a/n 直选仍然
// 有效，所以按钮上保留字母，老习惯不用改。
func (m *Model) renderApprovalButtons(v *approvalView) string {
	s := m.styler
	buttons := []struct {
		label string
		kind  style
	}{
		{"[y] 批准一次", styleAccent},
		{"[a] 本会话内都批准", styleAccent},
		{"[n] 拒绝", styleErr},
	}
	parts := make([]string, 0, len(buttons))
	for i, btn := range buttons {
		if i == v.focus {
			parts = append(parts, s.paint(styleStrong, "▸")+s.paint(btn.kind, btn.label)+s.paint(styleStrong, "◂"))
			continue
		}
		parts = append(parts, s.paint(styleMuted, btn.label))
	}
	return strings.Join(parts, "   ")
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
	if v.selectMode {
		return m.renderHunkSelectPanel()
	}
	s := m.styler
	var b strings.Builder
	// 截断时 +N −M 只是前 256 KB 的统计，必须写在脸上。
	suffix := ""
	if v.truncated {
		suffix = "，已截断"
	}
	fmt.Fprintf(&b, "%s %s%s\n", s.paint(styleWarn, "✎ 待审批"), s.paint(styleStrong, v.path),
		s.paint(styleMuted, fmt.Sprintf("（+%d −%d%s）", v.added, v.removed, suffix)))
	b.WriteString(strings.Repeat("─", 72))
	b.WriteByte('\n')

	lines, hasMore := renderDiff(v.diff, v.offset, m.diffRows())
	for _, line := range lines {
		b.WriteString(s.stylizeDiffLine(line))
		b.WriteByte('\n')
	}
	if hasMore {
		b.WriteString("… 还有更多（↓ / pgdn）\n")
	}
	if v.truncated {
		b.WriteString("[diff 过长已截断，批准前请先用编辑器核对]\n")
	}
	b.WriteString(s.paint(styleMuted, strings.Repeat("─", panelWidth)))
	b.WriteByte('\n')
	b.WriteString(m.renderApprovalButtons(v))
	b.WriteByte('\n')
	hint := "tab 切换 · enter 执行 · esc 收起 · ↑↓ 翻行 · [ctrl+r] 重连"
	if len(v.hunks) > 1 {
		hint += " · [v] 逐块"
	}
	b.WriteString(m.renderPanelHint(hint))
	return b.String()
}

// renderCommandPanel 逐 token 展示 argv，并写明 cwd / 后端 / 超时：用户批的
// 必须是「实际要跑的东西」，而不是一句「要不要执行命令」。
func (m *Model) renderCommandPanel() string {
	v := m.approval
	s := m.styler
	var b strings.Builder
	b.WriteString(s.paint(styleWarn, "⌘ 待审批命令") + "\n")
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
	b.WriteString(s.paint(styleMuted, strings.Repeat("─", panelWidth)))
	b.WriteByte('\n')
	b.WriteString(m.renderApprovalButtons(v))
	b.WriteByte('\n')
	b.WriteString(m.renderPanelHint("tab 切换 · enter 执行 · esc 收起 · [ctrl+r] 重连 · ctrl+c 退出"))
	return b.String()
}

// renderExternalPanel 展示一次外部工具调用。用户看不到 argv/diff，能判断的
// 只有「哪个来源的哪个工具、要带什么参数」——这三项必须完整可读。
func (m *Model) renderExternalPanel() string {
	v := m.approval
	s := m.styler
	var b strings.Builder
	b.WriteString(s.paint(styleWarn, "⇄ 待审批外部工具") + "\n")
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
	b.WriteString(s.paint(styleMuted, strings.Repeat("─", panelWidth)))
	b.WriteByte('\n')
	b.WriteString(m.renderApprovalButtons(v))
	b.WriteByte('\n')
	b.WriteString(s.paint(styleMuted, "tab 切换焦点 · enter 执行 · esc 收起（不裁决） · ctrl+c 退出"))
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
	// 用户正在往上翻时不能把视图拽回底部：scroll 跟着内容一起加，
	// 看到的还是原来那几行，只把「下面又来了几行」记进 unseen。
	// 停在底部（scroll==0）时自然继续跟尾。
	if m.scroll > 0 {
		m.scroll++
		m.unseen++
	}
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
