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
	// tag 用于区分来源：内置命令为空，「仅手动」技能标「技能」。把它们
	// 混在一张列表里，用户需要一眼看出哪条会改动会话状态、哪条只是
	// 往下一条任务的系统提示里塞正文。
	tag string
}

var commandHints = []commandHint{
	{name: "/models", usage: "", desc: "切换会话模型"},
	{name: "/model", usage: "[名字]", desc: "只给下一条任务指定模型"},
	{name: "/skill", usage: "[名字]", desc: "指定下一条任务加载的技能"},
	{name: "/mcp", usage: "", desc: "MCP server 与工具数"},
	{name: "/ps", usage: "", desc: "后台进程（含孤儿）"},
	{name: "/roots", usage: "", desc: "会话挂载的附加根"},
	{name: "/new", usage: "", desc: "新开一条会话（作为新标签，旧会话继续在后台跑）"},
	{name: "/sessions", usage: "", desc: "会话列表：切换、改名、删除"},
	{name: "/rename", usage: "[标题]", desc: "改当前会话标题（不带参数则填入输入行）"},
	{name: "/trust", usage: "", desc: "信任并加载项目级扩展（AGENTS.md / .jiaoag）"},
	{name: "/untrust", usage: "", desc: "撤销信任，不再加载项目级扩展"},
	{name: "/exit", usage: "", desc: "退出（等同 ctrl+c，/quit 同义）"},
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
	if m.isCommandName(prefix) {
		return nil
	}
	hints := m.completionHints()
	items := make([]commandHint, 0, len(hints))
	for _, c := range hints {
		if strings.HasPrefix(strings.ToLower(c.name), prefix) {
			items = append(items, c)
		}
	}
	return items
}

// completionHints 把内置命令与「仅手动」技能拼成候选列表。
//
// 技能名由服务端下发（skill.list），客户端不再扫目录：技能目录属于服务端
// 配置，可能含项目级 .jiaoag/skills，客户端本地的目录不代表实际生效的那份。
// 内置命令排在前面且重名时优先——技能目录是用户的、随时会变，不能让一个
// 叫 models 的技能把真命令顶掉。
func (m *Model) completionHints() []commandHint {
	out := make([]commandHint, 0, len(commandHints)+len(m.skillCatalog))
	out = append(out, commandHints...)
	for _, s := range m.skillCatalog {
		if !s.Manual {
			continue
		}
		name := "/" + s.Name
		if builtinCommandName(name) || skillHintExists(out, name) {
			continue
		}
		out = append(out, commandHint{
			name:  name,
			usage: "[问题]",
			desc:  s.Description,
			tag:   "技能",
		})
	}
	return out
}

func builtinCommandName(name string) bool {
	for _, c := range commandHints {
		if c.name == name {
			return true
		}
	}
	return false
}

func skillHintExists(hints []commandHint, name string) bool {
	for _, c := range hints {
		if c.name == name {
			return true
		}
	}
	return false
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

// isCommandName 报告 text 是否正好是某条命令（含仅手动技能）的完整名字。
func (m *Model) isCommandName(text string) bool {
	for _, c := range m.completionHints() {
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
		tag := padDisplay(items[i].tag, 4)
		raw := marker + name + usage + tag
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

// skillCommand 识别「/技能名 问题」形式的输入。
//
// 只有服务端标了 disable-model-invocation 的技能才有这个入口——它们本来
// 就是「模型不许自选、只能用户点名」。内置命令优先：技能目录是用户随时
// 会改的，不能让重名技能把真命令顶掉。
func (m *Model) skillCommand(text string) (name, rest string, ok bool) {
	cut := strings.IndexAny(text, " \t")
	head := text
	if cut >= 0 {
		head = text[:cut]
		rest = strings.TrimSpace(text[cut+1:])
	}
	if !strings.HasPrefix(head, "/") || builtinCommandName(head) {
		return "", "", false
	}
	name = strings.TrimPrefix(head, "/")
	if name == "" {
		return "", "", false
	}
	for _, s := range m.skillCatalog {
		if s.Manual && s.Name == name {
			return name, rest, true
		}
	}
	return "", "", false
}
