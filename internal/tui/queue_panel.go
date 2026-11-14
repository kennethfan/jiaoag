package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

const (
	// queuePanelHeight 是队列面板一次最多显示的任务行数。
	queuePanelHeight = 10
	// queuePromptWidth 是面板里提问的显示宽度：超出截断成省略号。
	queuePromptWidth = 60
)

// queuedTask 是界面里的一个排队任务。字段全部来自事件，不做本地猜测：
// 重连后按 seq 全量重放也能还原出同样的列表。
type queuedTask struct {
	ID     domain.TaskID
	Prompt string
	Since  time.Time
	// cancelRequested 表示取消请求已发出。任务真正离队要等 task.status 的
	// 终态事件：回执只说明请求被接受，此刻它可能已经被拉起。
	cancelRequested bool
}

// queuePanelView 是队列面板的交互状态。
type queuePanelView struct {
	selected     int
	confirmClear bool
}

// enqueueQueued 登记排队任务。回执帧与服务端事件都会到达，按 ID 去重：
// 先到者定下入队时间，后到者只补提问文本。
func (m *Model) enqueueQueued(id domain.TaskID, prompt string, at time.Time) {
	if id == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	if i := m.queueIndex(id); i >= 0 {
		if m.queue[i].Prompt == "" {
			m.queue[i].Prompt = prompt
		}
		return
	}
	m.queue = append(m.queue, queuedTask{ID: id, Prompt: prompt, Since: at})
}

// queueIndex 返回任务在队列里的下标，没有则 -1。
func (m *Model) queueIndex(id domain.TaskID) int {
	for i := range m.queue {
		if m.queue[i].ID == id {
			return i
		}
	}
	return -1
}

// removeQueued 把任务移出队列（被拉起或已终态）。选中下标必须夹回合法
// 范围，否则面板会指向一个不存在的行。
func (m *Model) removeQueued(id domain.TaskID) {
	i := m.queueIndex(id)
	if i < 0 {
		return
	}
	m.queue = append(m.queue[:i], m.queue[i+1:]...)
	m.clampQueueSelection()
}

// clampQueueSelection 把选中下标限制在 [0, len(queue)-1]。
func (m *Model) clampQueueSelection() {
	if m.queuePanel == nil {
		return
	}
	if m.queuePanel.selected >= len(m.queue) {
		m.queuePanel.selected = len(m.queue) - 1
	}
	if m.queuePanel.selected < 0 {
		m.queuePanel.selected = 0
	}
}

// requestCancelQueued 对指定排队任务发取消。已经发过的不重复发。
func (m *Model) requestCancelQueued(id domain.TaskID) tea.Cmd {
	i := m.queueIndex(id)
	if i < 0 || m.queue[i].cancelRequested {
		return nil
	}
	m.queue[i].cancelRequested = true
	m.appendLine(fmt.Sprintf("已请求取消排队任务 %s（等终态确认）", shortID(string(id))))
	return sendCmd(m.conn, protocol.TypeTaskCancel, protocol.TaskCancelPayload{
		SessionID: m.sessionID,
		TaskID:    string(id),
	})
}

// handleQueueKey 处理队列面板打开时的按键。面板是模态的：除下方这些键，
// 一律忽略，避免用户一边看队列一边把字符打进输入行。
func (m *Model) handleQueueKey(key string) (tea.Model, tea.Cmd) {
	v := m.queuePanel
	switch key {
	case "esc":
		// 确认态下 esc 只退确认，不关面板：避免一次误按丢掉清空提示。
		if v.confirmClear {
			v.confirmClear = false
			return m, nil
		}
		m.queuePanel = nil
		return m, nil
	case "up":
		if v.selected > 0 {
			v.selected--
		}
		return m, nil
	case "down":
		if v.selected < len(m.queue)-1 {
			v.selected++
		}
		return m, nil
	case "pgup":
		v.selected -= queuePanelHeight - 1
		if v.selected < 0 {
			v.selected = 0
		}
		return m, nil
	case "pgdown":
		v.selected += queuePanelHeight - 1
		if v.selected > len(m.queue)-1 {
			v.selected = len(m.queue) - 1
		}
		return m, nil
	case "x", "d":
		if len(m.queue) == 0 {
			return m, nil
		}
		return m, m.requestCancelQueued(m.queue[v.selected].ID)
	case "D":
		if len(m.queue) > 0 {
			v.confirmClear = true
		}
		return m, nil
	case "y":
		if !v.confirmClear {
			return m, nil
		}
		v.confirmClear = false
		// 每条取消都要有独立的终态确认，不能只改本地状态。
		var cmds []tea.Cmd
		for _, q := range m.queue {
			if cmd := m.requestCancelQueued(q.ID); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)
	}
	// 确认态下按其它键视为放弃：提示不能一直挂着。
	if v.confirmClear {
		v.confirmClear = false
	}
	return m, nil
}

// renderQueuePanel 画队列面板：标题、按 FIFO 顺序的任务行、底部键位提示。
func (m *Model) renderQueuePanel() string {
	var b strings.Builder
	fmt.Fprintf(&b, "排队任务 %d 个（按顺序执行）\n", len(m.queue))
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')

	// 固定高度下滚动跟随选中项，只显示它附近的一段。
	start := 0
	if m.queuePanel.selected >= queuePanelHeight {
		start = m.queuePanel.selected - queuePanelHeight + 1
	}
	end := start + queuePanelHeight
	if end > len(m.queue) {
		end = len(m.queue)
	}

	for i := start; i < end; i++ {
		q := m.queue[i]
		marker := "  "
		if i == m.queuePanel.selected {
			marker = "▶ "
		}
		wait := time.Since(q.Since).Round(time.Second)
		if wait < 0 {
			wait = 0
		}
		line := fmt.Sprintf("%s[%d] %s · 已等待 %s · %s",
			marker, i+1, shortID(string(q.ID)), wait, compactPrompt(q.Prompt))
		if q.cancelRequested {
			line += " · 已请求取消"
		}
		b.WriteString(strings.Join(wrapText(line, panelWidth), "\n"))
		b.WriteByte('\n')
	}

	if m.queuePanel.confirmClear {
		fmt.Fprintf(&b, "[确认] 再按 y 清空 %d 个排队任务，其它键取消\n", len(m.queue))
	} else {
		b.WriteString("[↑↓] 选择  [x] 取消选中  [D] 清空  [esc] 关闭\n")
	}
	return b.String()
}

// compactPrompt 把多行提问压成一行并截断，避免一条长 prompt 撑爆面板。
func compactPrompt(prompt string) string {
	one := strings.Join(strings.Fields(prompt), " ")
	if one == "" {
		return "（无提问）"
	}
	if utf8.RuneCountInString(one) > queuePromptWidth {
		one = string([]rune(one)[:queuePromptWidth]) + "…"
	}
	return one
}
