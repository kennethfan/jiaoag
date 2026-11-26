package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// keyMsg 把测试里的按键名翻成 bubbletea 的消息。只覆盖这条测试用到的键。
func keyMsg(name string) tea.KeyPressMsg {
	switch name {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "ctrl+n":
		return tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}
	case "ctrl+p":
		return tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	case "ctrl+w":
		return tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}
	}
	r := []rune(name)[0]
	return tea.KeyPressMsg{Code: r, Text: name}
}

// 状态栏是「在哪、在干什么、花了多少」三件事的唯一常驻出口，
// 这条测试盯住它别把工作目录或状态词弄丢。
func TestStatusBarShowsWorkspaceStateAndModel(t *testing.T) {
	m := newModel(t)
	m.opts.Workspace = "/tmp/some/repo"
	m.width = 120
	m.taskStatus = string(domain.TaskRunning)
	m.taskID = "tsk_7a408a1b2c3d"
	m.round = 3
	m.queue = []queuedTask{{ID: "tsk_1"}}
	m.tokens = 12400
	m.sessionProvider, m.sessionModel = "agnes", "agnes-3.0-flash"

	view := m.View().Content
	for _, want := range []string{"⌂ /tmp/some/repo", "[运行中]", "tsk_7a408a1b", "第 3 轮", "排队 1", "agnes/agnes-3.0-flash", "12.4k tok"} {
		if !strings.Contains(view, want) {
			t.Fatalf("状态栏缺少 %q：\n%s", want, view)
		}
	}
	// 分隔线必须跟视口等宽，窄终端下不能仍是 72。
	if !strings.Contains(view, strings.Repeat("-", 120)) {
		t.Fatalf("分隔线没有跟视口同宽：\n%s", view)
	}
}

// 窄屏时右侧从尾到头放弃：先丢 token/上下文读数，模型名留着——不知道谁在
// 回答就没法判断这句话该不该信。左侧的连接、目录、状态必须留着。
func TestStatusBarCollapsesRightOnNarrowScreen(t *testing.T) {
	m := newModel(t)
	m.opts.Workspace = "/tmp/repo"
	m.width = 60
	m.taskStatus = string(domain.TaskRunning)
	m.sessionProvider, m.sessionModel = "agnes", "agnes-3.0-flash"
	m.tokens = 12000
	m.contextTokens, m.contextBudget = 5000, 32000

	status := m.renderStatusBar(60, false)
	if !strings.Contains(status, "agnes-3.0-flash") {
		t.Fatalf("窄屏也要保留模型名：%q", status)
	}
	if strings.Contains(status, "ctx ") || strings.Contains(status, "12.0k tok") {
		t.Fatalf("窄屏应先丢用量读数：%q", status)
	}
	if !strings.Contains(status, "⌂ /tmp/repo") || !strings.Contains(status, "[运行中]") {
		t.Fatalf("窄屏丢了关键信息：%q", status)
	}
	if displayWidth(status) > 61 { // 60 列 + 换行
		t.Fatalf("状态栏超宽：%d 列", displayWidth(status))
	}
}

func TestHelpPanelTogglesAndListsKeys(t *testing.T) {
	m := newModel(t)
	m.width = 100
	m.conn = nil
	m.opts.Workspace = "/tmp/repo"
	m.sessionID = "ses_1"

	m.Update(keyMsg("?"))
	if !m.help {
		t.Fatal("? 应打开键位面板")
	}
	view := m.View().Content
	for _, want := range []string{"键位与状态", "ctrl+f", "/roots", "会话 ses_1", "工作区 /tmp/repo"} {
		if !strings.Contains(view, want) {
			t.Fatalf("帮助面板缺少 %q：\n%s", want, view)
		}
	}
	m.Update(keyMsg("esc"))
	if m.help {
		t.Fatal("esc 应关闭键位面板")
	}
}

// tab 移动焦点，enter 执行当前焦点：这是鼠标之外的唯一发现路径。
func TestApprovalButtonsTabAndEnter(t *testing.T) {
	m := newModel(t)
	m.conn = nil
	m.sessionID = "ses_1"
	m.approval = &approvalView{id: "apr_1", kind: string(domain.ApprovalDiff), path: "a.txt"}

	m.Update(keyMsg("tab"))
	if m.approval.focus != 1 {
		t.Fatalf("tab 后焦点 = %d，期望 1", m.approval.focus)
	}
	m.Update(keyMsg("shift+tab"))
	if m.approval.focus != 0 {
		t.Fatalf("shift+tab 后焦点 = %d，期望 0", m.approval.focus)
	}
	// 焦点到决定的映射单独断言：没有连接时 resolveApproval 会早退，
	// 这里测的是「按钮语义」，不是网络路径。
	for focus, want := range map[int]domain.Decision{
		0: domain.DecisionAllowOnce,
		1: domain.DecisionAllowSession,
		2: domain.DecisionDeny,
	} {
		if got := approvalDecision(focus); got != want {
			t.Fatalf("焦点 %d → %q，期望 %q", focus, got, want)
		}
	}
}

func TestApprovalEscCollapsesWithoutDeciding(t *testing.T) {
	m := newModel(t)
	m.conn = nil
	m.sessionID = "ses_1"
	m.approval = &approvalView{id: "apr_1", kind: string(domain.ApprovalDiff), path: "a.txt", diff: "@@ -1 +1 @@\n-a\n+b\n"}
	m.width = 100

	m.Update(keyMsg("esc"))
	if m.approval == nil || !m.approval.collapsed {
		t.Fatalf("esc 应只收起面板：%+v", m.approval)
	}
	view := m.View().Content
	if strings.Contains(view, "待审批 a.txt") {
		t.Fatalf("收起后不应再画卡片：\n%s", view)
	}
	if !strings.Contains(view, "[等待审批]") {
		t.Fatalf("收起后状态栏仍要显示等待审批：\n%s", view)
	}
	m.Update(keyMsg("esc"))
	if m.approval.collapsed {
		t.Fatal("再按 esc 应展开")
	}
}

func TestStylerRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	st := newStyler()
	if got := st.paint(styleErr, "boom"); got != "boom" {
		t.Fatalf("NO_COLOR 下不应有转义序列：%q", got)
	}
	if got := st.stylizeLine("› 你好"); got != "› 你好" {
		t.Fatalf("NO_COLOR 下对话行应原样输出：%q", got)
	}
}

func TestDisplayWidthCountsWideRunes(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"abc", 3},
		{"你好", 4},
		{"你a好", 5},
		{"\x1b[36mabc\x1b[0m", 3},
	}
	for _, c := range cases {
		if got := displayWidth(c.text); got != c.want {
			t.Fatalf("displayWidth(%q) = %d，期望 %d", c.text, got, c.want)
		}
	}
	if got := truncateDisplay("abcdef", 4); displayWidth(got) > 4 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateDisplay 结果 = %q", got)
	}
	if got := truncateMiddle("/Users/me/code/services/api", 20); displayWidth(got) > 20 || !strings.Contains(got, "…") {
		t.Fatalf("truncateMiddle 结果 = %q（宽 %d）", got, displayWidth(got))
	}
}

// 全屏时内容区必须按终端高度裁：否则状态栏会被历史推到屏幕外，
// 「钉底」就成了空话。
// 内容没占满一屏时也要把底部区推到终端最下沿：全屏模式下补空行，
// 否则输入行与状态栏会浮在中间，看起来像“贴着顶部”。
func TestFullscreenPadsShortContentToBottom(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.width, m.height = 80, 20
	m.sessionID = "ses_1"
	m.appendLine("只有两行")
	m.appendLine("历史")

	view := m.View().Content
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("短内容也应渲染满 20 行，实际 %d：\n%s", len(lines), view)
	}
	if lines[0] != "只有两行" {
		t.Fatalf("首行应是第一条历史：%q", lines[0])
	}
	if lines[2] != "" {
		t.Fatalf("历史之后应补空行，实际 %q", lines[2])
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "已连接") && !strings.Contains(last, "未连接") {
		t.Fatalf("末行应是状态栏：%q", last)
	}
	if !strings.Contains(lines[len(lines)-2], "› ") {
		t.Fatalf("输入行应紧贴状态栏：%q", lines[len(lines)-2])
	}
}

func TestFullscreenClipsContentToViewport(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.width, m.height = 80, 20
	m.sessionID = "ses_1"
	for i := 0; i < 100; i++ {
		m.appendLine("行")
	}
	view := m.View().Content
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("全屏渲染 %d 行，期望 20：\n%s", len(lines), view)
	}
	if !strings.Contains(view, "上方还有") {
		t.Fatalf("裁剪时应提示上方行数：\n%s", view)
	}
	// 最底一行必须是状态栏，输入行紧贴它上面（中间不再有分隔线）。
	last := lines[len(lines)-1]
	if !strings.Contains(last, "已连接") && !strings.Contains(last, "未连接") {
		t.Fatalf("末行不是状态栏：%q", last)
	}
	if !strings.Contains(lines[len(lines)-2], "› ") {
		t.Fatalf("输入行没有紧贴状态栏：%q", lines[len(lines)-2])
	}

	// pgup 往回翻；翻页期间来的新输出不能把视图拽回底部，
	// 只把行数记进 unseen 并在状态栏提示。
	m.Update(keyMsg("pgup"))
	if m.scroll == 0 {
		t.Fatal("pgup 应上翻")
	}
	before := m.scroll
	m.appendLine("新的一行")
	if m.scroll != before+1 {
		t.Fatalf("翻页时新输出应保持位置，scroll %d → %d", before, m.scroll)
	}
	if m.unseen != 1 {
		t.Fatalf("新输出应计入 unseen，实际 %d", m.unseen)
	}
	// end 回到底部并清掉未读计数。
	m.Update(keyMsg("end"))
	if m.scroll != 0 || m.unseen != 0 {
		t.Fatalf("end 应回底部，scroll=%d unseen=%d", m.scroll, m.unseen)
	}
}

// 内联模式不裁内容：那是它保留 scrollback 的意义。
// 长行会被终端折行，按条目数裁剪会把底部推出屏幕——这里断言长输出下
// 仍然恰好占满视口，且状态栏还在最底。
func TestFullscreenWrapsLongLinesWithinViewport(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.width, m.height = 40, 12
	m.sessionID = "ses_1"
	long := strings.Repeat("长", 120) // 40 列下每行折成 3 行
	for i := 0; i < 6; i++ {
		m.appendLine(long)
	}
	view := m.View().Content
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != 12 {
		t.Fatalf("长行折行后应恰好 12 行，实际 %d：\n%s", len(lines), view)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(stripANSI(last), "已连接") && !strings.Contains(stripANSI(last), "未连接") {
		t.Fatalf("末行应是状态栏：%q", last)
	}
	if lines[0] == "" {
		t.Fatal("视口顶部不应是空行（内容行数为偶数时会这样）")
	}
}

func TestInlineModeKeepsFullHistory(t *testing.T) {
	m := newModel(t)
	m.fullscreen = false
	m.width, m.height = 80, 20
	m.sessionID = "ses_1"
	for i := 0; i < 60; i++ {
		m.appendLine("行")
	}
	lines := strings.Split(strings.TrimRight(m.View().Content, "\n"), "\n")
	if len(lines) < 60 {
		t.Fatalf("内联模式丢了历史：%d 行", len(lines))
	}
}

// 键位提示并进状态栏：宽屏带提示，窄屏先丢提示，别挤掉模型与用量。
func TestStatusBarCarriesHintsWhenRoomAllows(t *testing.T) {
	m := newModel(t)
	m.opts.Workspace = "/tmp/repo"
	m.sessionID = "ses_1"
	m.sessionProvider, m.sessionModel = "agnes", "agnes-3.0-flash"
	m.tokens = 12000

	wide := stripANSI(m.renderStatusBar(140, true))
	if !strings.Contains(wide, "enter 发送") {
		t.Fatalf("宽屏状态栏应带键位提示：%q", wide)
	}
	if !strings.Contains(wide, "agnes-3.0-flash") {
		t.Fatalf("宽屏丢了模型：%q", wide)
	}
	if displayWidth(wide) > 141 {
		t.Fatalf("状态栏超宽：%d 列", displayWidth(wide))
	}

	narrow := stripANSI(m.renderStatusBar(60, true))
	if strings.Contains(narrow, "enter 发送") {
		t.Fatalf("窄屏应先丢键位提示：%q", narrow)
	}

	// 面板打开时由面板底栏负责键位，状态栏不再重复。
	panel := stripANSI(m.renderStatusBar(140, false))
	if strings.Contains(panel, "enter 发送") {
		t.Fatalf("面板态状态栏不该重复键位：%q", panel)
	}
}

// 输入行或状态栏本身超过一屏宽时，终端会折行——整帧就会比终端高一两行，
// alt screen 于是产生滚动backlog，往上滚能看到旧帧。全屏渲染必须先把
// 底部区也折成视觉行再排版，保证总行数恒等于终端高度。
func TestFullscreenNeverExceedsViewportHeight(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.width, m.height = 40, 12
	m.sessionID = "ses_1"
	m.input = strings.Repeat("很长的输入", 40)
	m.appendLine(strings.Repeat("历史", 60))

	view := m.View().Content
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != 12 {
		t.Fatalf("渲染 %d 行，期望恰好 12：\n%s", len(lines), view)
	}
	last := stripANSI(lines[len(lines)-1])
	if !strings.Contains(last, "已连接") && !strings.Contains(last, "未连接") {
		t.Fatalf("末行应是状态栏：%q", last)
	}
	if !strings.Contains(stripANSI(lines[len(lines)-2]), "› ") {
		t.Fatalf("输入行应紧贴状态栏：%q", stripANSI(lines[len(lines)-2]))
	}
}

// 全屏帧末尾不能带换行：渲染器会把多出来的空行算进帧高，终端滚动一行，
// 最后两行（输入框、状态栏）就会在那一瞬间被顶出屏幕。
func TestFullscreenViewHasNoTrailingNewline(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.width, m.height = 80, 16
	m.sessionID = "ses_1"
	for i := 0; i < 40; i++ {
		m.appendLine("一行不算短的历史内容，用来把视口撑满")
	}
	view := m.View().Content
	if strings.HasSuffix(view, "\n") {
		t.Fatalf("全屏帧不应以换行结尾：%q", view[len(view)-20:])
	}
	if got := strings.Count(view, "\n") + 1; got != 16 {
		t.Fatalf("全屏帧应为 16 行，实际 %d", got)
	}
}

// 流式输出里带换行时，帧高仍必须等于终端高度：否则每帧都会多出几行，
// 终端跟着滚动，输入框与状态栏在流式期间反复被顶出屏幕。
func TestFullscreenStreamWithNewlinesKeepsViewportHeight(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.width, m.height = 80, 20
	m.sessionID = "ses_1"
	m.taskStatus = string(domain.TaskRunning)
	m.stream = "第一段\n第二段\n第三段很长" + strings.Repeat("内容", 60) + "\n收尾"

	view := m.View().Content
	if strings.HasSuffix(view, "\n") {
		t.Fatalf("全屏帧不应以换行结尾")
	}
	if got := strings.Count(view, "\n") + 1; got != 20 {
		t.Fatalf("帧应为 20 行，实际 %d:\n%s", got, view)
	}
	lines := strings.Split(view, "\n")
	if !strings.Contains(lines[len(lines)-1], "已连接") && !strings.Contains(lines[len(lines)-1], "未连接") {
		t.Fatalf("末行应是状态栏：%q", lines[len(lines)-1])
	}
	if !strings.Contains(lines[len(lines)-2], "› ") {
		t.Fatalf("输入行应紧贴状态栏：%q", lines[len(lines)-2])
	}
}

// 底部区的高度不能随内容、会话或面板状态变化：分隔线 + 输入行 + 状态栏
// 永远三行，内容再长也只影响内容区的可见行数。
func TestDockHeightIsConstant(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *Model)
	}{
		{"无会话", func(m *Model) { m.sessionID = "" }},
		{"有会话", func(m *Model) { m.sessionID = "ses_1" }},
		{"面板打开", func(m *Model) { m.sessionID = "ses_1"; m.help = true }},
		{"超长输入", func(m *Model) {
			m.sessionID = "ses_1"
			m.input = strings.Repeat("很长的输入", 40)
		}},
		{"大量历史", func(m *Model) {
			m.sessionID = "ses_1"
			for i := 0; i < 200; i++ {
				m.appendLine("历史行 " + strings.Repeat("内容", 30))
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newModel(t)
			m.fullscreen = true
			m.width, m.height = 80, 24
			m.opts.Workspace = "/tmp/repo"
			c.setup(m)

			lines := strings.Split(m.View().Content, "\n")
			if len(lines) != 24 {
				t.Fatalf("帧应为 24 行，实际 %d", len(lines))
			}
			// 底部三行的顺序固定：分隔线、输入行、状态栏。
			if got := stripANSI(lines[21]); !strings.HasPrefix(got, "-") {
				t.Fatalf("倒数第三行应是分隔线：%q", got)
			}
			if got := stripANSI(lines[22]); !strings.HasPrefix(got, "› ") {
				t.Fatalf("倒数第二行应是输入行：%q", got)
			}
			if got := stripANSI(lines[23]); !strings.Contains(got, "已连接") && !strings.Contains(got, "未连接") {
				t.Fatalf("末行应是状态栏：%q", got)
			}
		})
	}
}

// 会话标题只在顶部标题栏里出现一次：状态栏不再重复，导航信息不占两份。
func TestStatusBarHasNoSessionLabel(t *testing.T) {
	m := newModel(t)
	m.sessionTitle = "修登录超时"
	bar := stripANSI(m.renderStatusBar(140, false))
	if strings.Contains(bar, "▤") {
		t.Fatalf("状态栏不该再重复会话标题：%s", bar)
	}
}

func TestStatusBarShowsModelTokensAndContext(t *testing.T) {
	m := newModel(t)
	m.sessionProvider, m.sessionModel = "agnes", "agnes-3.0-flash"
	m.tokens = 12400
	m.contextTokens = 5200
	m.contextBudget = 32000

	bar := stripANSI(m.renderStatusBar(140, false))
	for _, want := range []string{"agnes/agnes-3.0-flash", "12.4k tok", "ctx 5.2k/32k"} {
		if !strings.Contains(bar, want) {
			t.Fatalf("状态栏缺少 %q：%s", want, bar)
		}
	}

	// 预算未知时只报占用，不编一个分母。
	m.contextBudget = 0
	bar = stripANSI(m.renderStatusBar(140, false))
	if !strings.Contains(bar, "ctx 5.2k") || strings.Contains(bar, "ctx 5.2k/") {
		t.Fatalf("无预算时不该带分母：%s", bar)
	}
}

func TestAssistantStartUpdatesContextUsage(t *testing.T) {
	m := newModel(t)
	payload := json.RawMessage(`{"round":2,"model":"m","context_tokens":6400,"context_budget":32000}`)
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    string(domain.EventAssistantStart),
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("构造事件帧: %v", err)
	}
	m.handleEnvelope(env)
	if m.contextTokens != 6400 || m.contextBudget != 32000 {
		t.Fatalf("上下文读数 = %d/%d", m.contextTokens, m.contextBudget)
	}

	// 重连全量重放前会 resetView：读数跟着清，避免拿旧会话的数字骗人。
	m.resetView()
	if m.contextTokens != 0 || m.contextBudget != 0 {
		t.Fatalf("resetView 没清上下文读数：%d/%d", m.contextTokens, m.contextBudget)
	}
}
