package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// sessionsPanelView 是 /sessions 打开的会话管理面板。
//
// 它只列当前工作区的会话：跨项目把别的仓库的对话拉进来，切过去以后工作区
// 对不上，工具路径会全错。服务端按会话的工作区筛选，客户端不做这件事。
type sessionsPanelView struct {
	items    []protocol.SessionSummaryPayload
	selected int
	offset   int
	// confirmID 是已经按过一次 d 的会话：删除要两次确认，第二次才真删。
	confirmID string
}

// sessionsPanelHeight 是一屏放几条会话，每条占两行（标题行 + 统计行）。
const sessionsPanelHeight = 4

func newSessionsPanel(items []protocol.SessionSummaryPayload, current string) *sessionsPanelView {
	v := &sessionsPanelView{items: items}
	for i, item := range items {
		if item.ID == current {
			v.selected = i
			break
		}
	}
	return v
}

// handleSessionsKey 处理 /sessions 面板上的按键。
func (m *Model) handleSessionsKey(key string) (tea.Model, tea.Cmd) {
	v := m.sessionsPanel
	if len(v.items) == 0 {
		if key == "esc" || key == "enter" {
			m.sessionsPanel = nil
		}
		return m, nil
	}
	item := v.items[v.selected]
	switch key {
	case "esc":
		m.sessionsPanel = nil
		return m, nil
	case "up", "k":
		if v.selected > 0 {
			v.selected--
		}
		v.confirmID = ""
	case "down", "j":
		if v.selected < len(v.items)-1 {
			v.selected++
		}
		v.confirmID = ""
	case "enter":
		return m.switchSession(item)
	case "t":
		m.beginRename(item)
		return m, nil
	case "d":
		return m.deleteSession(item)
	}
	if v.selected < v.offset {
		v.offset = v.selected
	}
	if v.selected >= v.offset+sessionsPanelHeight {
		v.offset = v.selected - sessionsPanelHeight + 1
	}
	return m, nil
}

// switchSession 就地切到另一条会话：停掉旧订阅、清空视图、从 seq=0 回放新会话。
//
// 旧订阅必须先退：服务端的 subs 是按会话 id 分键的，不退订就会两条流同时
// 往一条连接上推，事件互相串台。
func (m *Model) switchSession(item protocol.SessionSummaryPayload) (tea.Model, tea.Cmd) {
	m.sessionsPanel = nil
	if item.ID == m.sessionID {
		return m, nil
	}
	// 打开成标签而不是「换掉当前会话」：有任务在跑的会话照样切过去看，
	// 它在后台继续跑，标签上会显示进度。切换本身不退订旧会话。
	m.openTab(item.ID, item.Title, item.Provider, item.Model)
	if tab := m.currentTab(); tab != nil && tab.ID == item.ID {
		tab.Status = ""
	}
	return m.activateTab(item.ID, "")
}

// beginRename 把标题填进输入行，回车保存。复用输入行而不是做行内编辑：
// 中文输入法与光标处理都已经在那里了。
func (m *Model) beginRename(item protocol.SessionSummaryPayload) {
	m.sessionsPanel = nil
	m.renamingSession = item.ID
	m.input = item.Title
	m.appendLine("输入新标题后按 enter 保存（esc 取消）")
}

// deleteSession 第一次 d 只提醒，同一行再按一次才发删除。
func (m *Model) deleteSession(item protocol.SessionSummaryPayload) (tea.Model, tea.Cmd) {
	v := m.sessionsPanel
	if item.ActiveTasks > 0 {
		m.appendLine(fmt.Sprintf("⚠ %s 还有 %d 个任务没结束，不能删除", shortID(item.ID), item.ActiveTasks))
		return m, nil
	}
	if v.confirmID != item.ID {
		v.confirmID = item.ID
		m.appendLine(fmt.Sprintf("再按一次 d 永久删除 %s（含任务、事件与审计，无法撤销）", shortID(item.ID)))
		return m, nil
	}
	v.confirmID = ""
	return m, sendCmd(m.conn, protocol.TypeSessionDelete, protocol.SessionDeletePayload{
		SessionID: item.ID,
	})
}

// finishRename 提交输入行里的标题。
func (m *Model) finishRename(title string) tea.Cmd {
	id := m.renamingSession
	m.renamingSession = ""
	m.input = ""
	return sendCmd(m.conn, protocol.TypeSessionRename, protocol.SessionRenamePayload{
		SessionID: id,
		Title:     title,
	})
}

// switchSessionCmd 先退订旧会话、再订阅新会话，必要时顺带要一份技能清单。
//
// 三帧在一个命令里顺序写：bubbletea 的 Batch 是并发执行的，两条命令各写一帧
// 会让 wsjson 在同一个连接上并发写，帧会交错（实测会导致技能请求被丢弃）。
func (m *Model) switchSessionCmd(oldID string, sub protocol.EventSubscribePayload) tea.Cmd {
	conn := m.conn
	catalog := m.catalogToFetch(sub.SessionID)
	return func() tea.Msg {
		if conn == nil {
			return closedMsg{err: errors.New("未连接到服务端")}
		}
		if oldID != "" {
			env, err := protocol.New(protocol.TypeEventUnsubscribe, protocol.EventUnsubscribePayload{SessionID: oldID})
			if err != nil {
				return connectErrMsg{err: err}
			}
			if err := conn.Send(env); err != nil {
				return closedMsg{err: err}
			}
		}
		env, err := protocol.New(protocol.TypeEventSubscribe, sub)
		if err != nil {
			return connectErrMsg{err: err}
		}
		if err := conn.Send(env); err != nil {
			return closedMsg{err: err}
		}
		if catalog != nil {
			env, err := protocol.New(protocol.TypeSkillList, *catalog)
			if err != nil {
				return connectErrMsg{err: err}
			}
			if err := conn.Send(env); err != nil {
				return closedMsg{err: err}
			}
		}
		return nil
	}
}

// catalogToFetch 决定这次切会话要不要顺带拉技能清单：同一个会话只拉一次，
// 正在拉时也不重复请求。技能名用于斜杠命令补全。
func (m *Model) catalogToFetch(sessionID string) *protocol.SkillListPayload {
	if m.conn == nil || sessionID == "" {
		return nil
	}
	if m.skillFetching || m.skillCatalogSession == sessionID {
		return nil
	}
	m.skillFetching = true
	return &protocol.SkillListPayload{SessionID: sessionID}
}

func (m *Model) renderSessionsPanel() string {
	v := m.sessionsPanel
	var b strings.Builder
	fmt.Fprintf(&b, "⇄ 会话（%d 条，工作区 %s）\n", len(v.items), m.abbrevWorkspace())
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	if len(v.items) == 0 {
		b.WriteString("这个工作区还没有会话\n")
	}
	end := v.offset + sessionsPanelHeight
	if end > len(v.items) {
		end = len(v.items)
	}
	for i := v.offset; i < end; i++ {
		item := v.items[i]
		b.WriteString(strings.Join(wrapText(sessionLine(item, i == v.selected, item.ID == m.sessionID, v.confirmID == item.ID), panelWidth), "\n"))
		b.WriteByte('\n')
	}
	b.WriteString("[↑↓/jk] 选择  [enter] 切换  [t] 改名  [d] 删除  [esc] 关闭")
	return b.String()
}

// sessionLine 把一条会话渲染成两行：标识行 + 统计行。
func sessionLine(item protocol.SessionSummaryPayload, selected, current, confirm bool) string {
	marker := "  "
	if selected {
		marker = "▶ "
	}
	title := item.Title
	if title == "" {
		title = "(未命名)"
	}
	if current {
		title += " (当前)"
	}
	flags := []string{fmt.Sprintf("%d 任务", item.Tasks)}
	if item.ActiveTasks > 0 {
		flags = append(flags, fmt.Sprintf("%d 进行中", item.ActiveTasks))
	}
	if confirm {
		flags = append(flags, "再按 d 删除")
	}
	head := fmt.Sprintf("%s%s  %s  %s  %s", marker, title, shortID(item.ID),
		strings.Join(flags, "  "), sessionWhen(item.UpdatedAt))

	detail := []string{}
	if item.Model != "" {
		detail = append(detail, modelLabel(item.Provider, item.Model))
	}
	if item.PromptTokens+item.CompletionTokens > 0 {
		detail = append(detail, fmt.Sprintf("%s/%s tok", humanTokens(item.PromptTokens), humanTokens(item.CompletionTokens)))
	}
	tail := ""
	if len(detail) > 0 {
		tail = "\n      " + strings.Join(detail, " · ")
	}
	return head + tail
}

// sessionWhen 把时间压成「今天看时刻、更早看日期」：面板里只需要区分新旧。
func sessionWhen(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	t := time.UnixMilli(ms)
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("01-02")
}

// forgetSession 从面板里摘掉一条（删除成功后），保持选中位置不越界。
func (m *Model) forgetSession(id string) {
	v := m.sessionsPanel
	if v == nil {
		return
	}
	kept := v.items[:0]
	for _, item := range v.items {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	v.items = kept
	if v.selected >= len(v.items) {
		v.selected = len(v.items) - 1
	}
	if v.selected < 0 {
		v.selected = 0
	}
	if v.offset > v.selected {
		v.offset = v.selected
	}
	v.confirmID = ""
}
