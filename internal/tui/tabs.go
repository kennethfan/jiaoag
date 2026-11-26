package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// sessionTab 是同窗口打开的一条会话。
//
// 它只存「标签上要显示什么」：标题、最近状态、待审批数、未读数。对话正文
// 不在这里——切回来时从事件重放重建，因为事件日志本来就是唯一事实来源，
// 另存一份缓冲只会两边不一致。
type sessionTab struct {
	ID, Title string
	// Status 是 domain.TaskStatus 的字面值，空表示这条会话还没跑过任务。
	Status string
	// Pending 是未裁决的审批数，标签上显示 !。
	Pending int
	// Unread 是切走之后收到的新消息数，切回去时清零。
	Unread   int
	Provider string
	Model    string
}

// tabIndex 返回会话所在标签的下标，没打开时返回 -1。
func (m *Model) tabIndex(id string) int {
	for i, t := range m.tabs {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// openTab 登记一条会话为标签（已存在则就地更新标题与模型）。
func (m *Model) openTab(id, title, provider, model string) *sessionTab {
	if id == "" {
		return nil
	}
	if i := m.tabIndex(id); i >= 0 {
		t := m.tabs[i]
		t.Title, t.Provider, t.Model = title, provider, model
		return t
	}
	t := &sessionTab{ID: id, Title: title, Provider: provider, Model: model}
	m.tabs = append(m.tabs, t)
	return t
}

// currentTab 返回当前会话的标签。
func (m *Model) currentTab() *sessionTab {
	if i := m.tabIndex(m.sessionID); i >= 0 {
		return m.tabs[i]
	}
	return nil
}

// syncCurrentTab 把视图状态写回标签，供切走后在标签行上继续显示。
func (m *Model) syncCurrentTab() {
	t := m.currentTab()
	if t == nil {
		return
	}
	// 空值不覆盖：本地可能还不知道标题（改名来自别的客户端时靠事件补），
	// 用空串盖掉会先把标签名抹白再等下一次事件。
	if m.sessionTitle != "" {
		t.Title = m.sessionTitle
	}
	if m.sessionProvider != "" {
		t.Provider = m.sessionProvider
	}
	if m.sessionModel != "" {
		t.Model = m.sessionModel
	}
	t.Status = m.taskStatus
}

// setTabTitle 让改名立刻反映到标签行（事件与回帧两条路径都会调）。
func (m *Model) setTabTitle(id, title string) {
	if i := m.tabIndex(id); i >= 0 {
		m.tabs[i].Title = title
	}
}

// removeTab 关掉标签（不负责退订，调用方按需发 event.unsubscribe）。
func (m *Model) removeTab(id string) {
	if i := m.tabIndex(id); i >= 0 {
		m.tabs = append(m.tabs[:i], m.tabs[i+1:]...)
	}
}

// activateTab 把视图切到某个标签：清空当前视图、按事件从 seq=0 重放。
//
// oldID 非空时先退订那条会话（关标签时用）；普通切换故意不退订——后台
// 会话继续接收事件，标签上的状态与徽标才不会停在切换那一刻。
func (m *Model) activateTab(id, oldID string) (tea.Model, tea.Cmd) {
	i := m.tabIndex(id)
	if i < 0 {
		return m, nil
	}
	// 先把当前会话登记成标签，再切走：否则第一次切换会把它弄丢，
	// 后台任务就再也回不去了。
	if m.sessionID != "" && m.sessionID != id {
		m.openTab(m.sessionID, m.sessionTitle, m.sessionProvider, m.sessionModel)
		m.syncCurrentTab()
	}
	t := m.tabs[i]
	m.resetView()
	m.sessionID = id
	m.sessionTitle, m.sessionProvider, m.sessionModel = t.Title, t.Provider, t.Model
	m.taskStatus = t.Status
	t.Unread = 0
	m.appendLine(sessionHeadline("切换到会话", t.Title))
	return m, switchSessionCmd(m.conn, oldID, protocol.EventSubscribePayload{
		SessionID: id,
		FromSeq:   0,
	})
}

// cycleTab 按方向在标签之间循环：delta 为 +1 下一个、-1 上一个。
func (m *Model) cycleTab(delta int) (tea.Model, tea.Cmd) {
	if len(m.tabs) < 2 {
		return m, nil
	}
	cur := m.tabIndex(m.sessionID)
	if cur < 0 {
		cur = 0
	}
	next := (cur + delta + len(m.tabs)) % len(m.tabs)
	return m.activateTab(m.tabs[next].ID, "")
}

// closeCurrentTab 关掉当前标签：退订它，切到相邻的一条。
//
// 最后一条不允许关——否则界面没有会话可显示，还得重新建一条。
func (m *Model) closeCurrentTab() (tea.Model, tea.Cmd) {
	if len(m.tabs) <= 1 {
		m.appendLine("⚠ 至少保留一个会话标签；想清除历史用 /sessions 删除")
		return m, nil
	}
	id := m.sessionID
	idx := m.tabIndex(id)
	if idx < 0 {
		return m, nil
	}
	m.removeTab(id)
	next := m.tabs[idx%len(m.tabs)].ID
	// 先把「当前会话」清掉，否则 activateTab 会把刚关掉的那条又登记回来。
	m.sessionID = ""
	return m.activateTab(next, id)
}

// noteBackgroundEvent 处理不属于当前会话的事件：只更新标签上的徽标，
// 不动对话区。这样后台任务跑着也能从标签行看到进展与待审批。
func (m *Model) noteBackgroundEvent(env protocol.Envelope) tea.Cmd {
	i := m.tabIndex(env.SessionID)
	if i < 0 {
		return nil
	}
	t := m.tabs[i]
	var p protocol.StreamEventPayload
	if err := env.Decode(&p); err != nil {
		return nil
	}
	switch p.Kind {
	case string(domain.EventTaskStatus):
		var s domain.TaskStatusPayload
		if json.Unmarshal(p.Payload, &s) == nil {
			t.Status = string(s.To)
		}
	case string(domain.EventTaskQueued):
		t.Status = string(domain.TaskQueued)
	case string(domain.EventApprovalRequested):
		t.Pending++
	case string(domain.EventApprovalResolved):
		if t.Pending > 0 {
			t.Pending--
		}
	case string(domain.EventAssistantMessage), string(domain.EventToolResult):
		t.Unread++
	}
	return nil
}

// renderTabStrip 画出标签行。只有一条会话时不画——单会话没有「切换」可言，
// 多一行只会挤掉内容。
// renderSingleTabHeader 画单会话时的顶部标题栏。
//
// 用「▤ 标题」而不是「[1 标题]」：它此刻不是可选中的标签，而是「你在哪个
// 会话里」的常驻读数。状态标记照旧跟在后面，待审批/运行中一眼可见。
func (m *Model) renderSingleTabHeader(width int) string {
	t := m.currentTab()
	if t == nil {
		return ""
	}
	title := t.Title
	if title == "" {
		title = "(未命名)"
	}
	line := m.styler.paint(styleMuted, "▤ ") + m.styler.paint(styleAccent, truncateDisplay(title, 32))
	if marker := strings.TrimSpace(tabMarker(t)); marker != "" {
		line += " " + m.styler.paint(styleWarn, marker)
	}
	if displayWidth(line) > width {
		line = truncateDisplay(line, width)
	}
	return line
}

func (m *Model) renderTabStrip(width int) string {
	if len(m.tabs) == 0 {
		return ""
	}
	// 只有一条会话时也占一行，但画成标题栏而不是标签：历史滚过一屏后，
	// 对话区顶部那条「已进入会话：…」会随之滚走，标题就再也看不到了。
	if len(m.tabs) == 1 {
		return m.renderSingleTabHeader(width)
	}
	labels := make([]string, len(m.tabs))
	current := 0
	for i, t := range m.tabs {
		title := t.Title
		if title == "" {
			title = "(未命名)"
		}
		label := fmt.Sprintf("[%d %s%s]", i+1, truncateDisplay(title, 14), tabMarker(t))
		if t.ID == m.sessionID {
			current = i
			label = m.styler.paint(styleAccent, label)
		} else {
			label = m.styler.paint(styleMuted, label)
		}
		labels[i] = label
	}
	// 会话多的时候标签行放不下：从当前标签向两边展开，两端用 … 与 +N 表示
	// 还有多少条。当前标签永远可见——它是你正在看的那条。
	render := func(start, end int) string {
		parts := make([]string, 0, end-start+2)
		if start > 0 {
			parts = append(parts, m.styler.paint(styleMuted, "…"))
		}
		parts = append(parts, labels[start:end]...)
		if end < len(labels) {
			parts = append(parts, m.styler.paint(styleMuted, fmt.Sprintf("+%d", len(labels)-end)))
		}
		return strings.Join(parts, "  ")
	}
	start, end := current, current+1
	for {
		cand := render(start, end)
		if displayWidth(cand) > width {
			break
		}
		switch {
		case end < len(labels) && displayWidth(render(start, end+1)) <= width:
			end++
		case start > 0 && displayWidth(render(start-1, end)) <= width:
			start--
		default:
			return cand
		}
	}
	line := render(start, end)
	if displayWidth(line) > width {
		line = truncateDisplay(line, width)
	}
	return line
}

// tabMarker 是标签上的状态标记：待审批优先（最需要人），其次运行中，
// 然后是排队与失败，最后才是未读数。
func tabMarker(t *sessionTab) string {
	switch {
	case t.Pending > 0:
		return " !"
	case t.Status == string(domain.TaskRunning):
		return " ●"
	case t.Status == string(domain.TaskQueued):
		return " ◌"
	case t.Status == string(domain.TaskFailed):
		return " ✖"
	case t.Unread > 0:
		return fmt.Sprintf(" ·%d", t.Unread)
	}
	return ""
}

// restoreWorkspaceSessions 把本目录的历史会话全部开成标签，并接到最近的一条上。
//
// 启动时新建会话会让历史变成看不见的孤儿：每次开一个窗口就多一条，旧的那条
// 只有去 /sessions 里才找得到。所以默认改成「恢复全部」，新会话用 /new 显式开。
func (m *Model) restoreWorkspaceSessions(sessions []protocol.SessionSummaryPayload) tea.Cmd {
	if len(sessions) == 0 {
		m.appendLine("这个目录还没有会话，已新建")
		return sendCmd(m.conn, protocol.TypeSessionCreate, protocol.SessionCreatePayload{
			Workspace: m.opts.Workspace,
			Roots:     m.opts.Roots,
			Provider:  m.opts.Provider,
			Model:     m.opts.Model,
		})
	}
	m.resetView()
	// 服务端按 updated_at 倒序给：标签按时间正序摆（新的在右），当前取最新那条。
	// 有未结束任务的那条直接标成运行中——Tab 上要能看出后台还在干活。
	for i := len(sessions) - 1; i >= 0; i-- {
		s := sessions[i]
		t := m.openTab(s.ID, s.Title, s.Provider, s.Model)
		if t != nil && s.ActiveTasks > 0 {
			t.Status = string(domain.TaskRunning)
		}
	}
	cur := sessions[0]
	m.sessionID = cur.ID
	m.sessionTitle = cur.Title
	m.sessionProvider, m.sessionModel = cur.Provider, cur.Model
	m.taskStatus = ""
	if cur.ActiveTasks > 0 {
		m.taskStatus = string(domain.TaskRunning)
	}
	m.status = fmt.Sprintf("会话 %s · %s", cur.ID, cur.Workspace)
	m.appendLine(sessionHeadline("继续会话", cur.Title))
	if cur.Model != "" {
		m.appendLine("模型：" + modelLabel(cur.Provider, cur.Model))
	}
	if len(sessions) > 1 {
		m.appendLine(fmt.Sprintf("⋯ 本目录有 %d 条会话，已全部开成标签（ctrl+n/ctrl+p 切换，/new 新开）", len(sessions)))
	}
	return sendCmd(m.conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: cur.ID,
		FromSeq:   0,
	})
}
