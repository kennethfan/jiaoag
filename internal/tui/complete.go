package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// commandHint 是斜杠命令的单一事实来源：输入补全与 ? 帮助面板都从这里取。
//
// usage 只用于展示（例如 “[标题]”），选中后只把命令名填进输入行——参数
// 交给用户继续打，或由对应面板完成。命令多了以后最怕两处列表不同步，
// 所以这里刻意不再维护第二份清单。
type commandHint struct {
	name  string
	usage string
	desc  string
}

var commandHints = []commandHint{
	{"/models", "", "切换会话模型"},
	{"/model", "[名字]", "只给下一条任务指定模型"},
	{"/skill", "[名字]", "指定下一条任务加载的技能"},
	{"/mcp", "", "MCP server 与工具数"},
	{"/roots", "", "会话挂载的附加根"},
	{"/new", "", "新开一条会话（作为新标签，旧会话继续在后台跑）"},
	{"/sessions", "", "会话列表：切换、改名、删除"},
	{"/rename", "[标题]", "改当前会话标题（不带参数则填入输入行）"},
	{"/trust", "", "信任并加载项目级扩展（AGENTS.md / .jiaoag）"},
	{"/untrust", "", "撤销信任，不再加载项目级扩展"},
	{"/exit", "", "退出（等同 ctrl+c，/quit 同义）"},
}

// completionMaxRows 是补全浮层的默认高度上限。
//
// 它必须参与底部区的高度预算：全屏时帧高要恒等于终端高度，浮层多占
// 一行，内容区就少一行，否则状态栏会被顶出屏幕。
const completionMaxRows = 6

// visibleCompletion 返回当前输入应当展示的候选。
//
// 只在「第一个 token、还没打空格」时出现：一旦进入参数区（出现空格），
// 命令名已经确定，再挡着屏幕就是噪音。空列表、面板打开或用户按过 esc
// 时也都不展示。
func (m *Model) visibleCompletion() []commandHint {
	if m.completionHidden || m.renamingSession != "" {
		return nil
	}
	text := m.input
	if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \t") {
		return nil
	}
	prefix := strings.ToLower(text)
	// 打全了就收起浮层：此刻 enter 的语义是执行这条命令。也避免 /model
	// 同时是 /models 的前缀时，浮层高亮停在另一条上造成误读。
	if isCommandName(prefix) {
		return nil
	}
	items := make([]commandHint, 0, len(commandHints))
	for _, c := range commandHints {
		if strings.HasPrefix(strings.ToLower(c.name), prefix) {
			items = append(items, c)
		}
	}
	return items
}

// resetCompletion 在输入变化后清掉「用户按 esc 关过」的记忆，并把选中项
// 拉回第一条：继续打字意味着重新挑选。
func (m *Model) resetCompletion() {
	m.completionSel = 0
	m.completionHidden = false
}

// handleCompletionKey 处理浮层打开时的导航键。
//
// 只吃方向键、tab、enter、esc：j/k 在这里是普通字符（用户正在输入命令名），
// 不能像面板那样当导航键，否则永远打不出以 j/k 开头的东西。
func (m *Model) handleCompletionKey(key string) (tea.Cmd, bool) {
	items := m.visibleCompletion()
	if len(items) == 0 {
		return nil, false
	}
	switch key {
	case "up":
		m.completionSel = (m.completionSel - 1 + len(items)) % len(items)
		return nil, true
	case "down":
		m.completionSel = (m.completionSel + 1) % len(items)
		return nil, true
	case "tab":
		m.applyCompletion(items)
		return nil, true
	case "esc":
		// 只关浮层，保留输入；第二次 esc 才走取消任务 / 退出。
		m.completionHidden = true
		return nil, true
	case "enter":
		// 浮层还在说明输入只是前缀：enter 只补全，不执行。避免输入一个
		// 斜杠随手回车就把第一条命令（可能是 /exit）跑掉；打全命令名时
		// 浮层已经收起，enter 会走正常的执行分支。
		m.applyCompletion(items)
		return nil, true
	}
	return nil, false
}

// isCommandName 报告 text 是否正好是某条命令的完整名字。
func isCommandName(text string) bool {
	for _, c := range commandHints {
		if c.name == text {
			return true
		}
	}
	return false
}

// applyCompletion 把选中候选填进输入行。补全后浮层自然消失（输入已等于
// 命令名），再按一次 enter 才执行。
func (m *Model) applyCompletion(items []commandHint) {
	sel := m.completionSel
	if sel < 0 || sel >= len(items) {
		sel = 0
	}
	m.input = items[sel].name
	m.completionSel = 0
	m.completionHidden = false
}

// renderCompletion 把候选画成贴在输入行上方的浮层。
func (m *Model) renderCompletion(width int) []string {
	items := m.visibleCompletion()
	if len(items) == 0 {
		return nil
	}
	max := completionMaxRows
	if m.height > 0 {
		switch {
		case m.height <= 8:
			max = 2
		case m.height <= 14:
			max = 3
		case m.height <= 20:
			max = 4
		}
	}
	if max > len(items) {
		max = len(items)
	}
	sel := m.completionSel
	if sel < 0 || sel >= len(items) {
		sel = 0
	}
	start := 0
	if sel >= max {
		start = sel - max + 1
	}
	lines := make([]string, 0, max)
	for i := start; i < start+max && i < len(items); i++ {
		marker := "  "
		style := styleMuted
		if i == sel {
			marker = "▶ "
			style = styleAccent
		}
		name := padDisplay(items[i].name, 10)
		usage := padDisplay(items[i].usage, 8)
		raw := marker + name + usage
		descWidth := width - displayWidth(raw) - 2
		desc := ""
		if descWidth > 6 {
			desc = truncateDisplay(items[i].desc, descWidth)
		}
		if desc != "" {
			raw += "  " + desc
		}
		lines = append(lines, m.styler.paint(style, strings.TrimRight(raw, " ")))
	}
	return lines
}
