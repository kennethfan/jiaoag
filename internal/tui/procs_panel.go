package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// procsPanelView 是 /ps 打开的后台进程面板。
//
// 它刻意列出**所有会话**的进程：孤儿进程恰恰属于那些已经没有客户端在看的
// 会话，只看当前会话就永远发现不了它们。
type procsPanelView struct {
	items    []protocol.ProcInfo
	selected int
	offset   int
	// confirm 记录「已经按过一次」的动作：d 删除、x 终止都要两步，
	// 因为这两件事都不可撤销（x 还会动系统里的进程）。
	confirm string
}

// procsPanelHeight 是一屏放几行进程。每行一条命令，太长会折行，所以
// 这个数字是保守值。
const procsPanelHeight = 10

func newProcsPanel(items []protocol.ProcInfo) *procsPanelView {
	return &procsPanelView{items: items}
}

func (v *procsPanelView) current() *protocol.ProcInfo {
	if v.selected < 0 || v.selected >= len(v.items) {
		return nil
	}
	return &v.items[v.selected]
}

func (m *Model) handleProcsKey(key string) (tea.Model, tea.Cmd) {
	v := m.procsPanel
	switch key {
	case "esc":
		m.procsPanel = nil
		return m, nil
	case "up", "k":
		if v.selected > 0 {
			v.selected--
		}
		v.confirm = ""
	case "down", "j":
		if v.selected < len(v.items)-1 {
			v.selected++
		}
		v.confirm = ""
	case "r":
		v.confirm = ""
		return m, sendCmd(m.conn, protocol.TypeProcList, protocol.ProcListPayload{})
	case "s":
		p := v.current()
		if p == nil || p.Status != "running" {
			m.appendLine("⚠ 只有运行中的进程可以停止；孤儿进程用 x 终止")
			return m, nil
		}
		v.confirm = ""
		return m, sendCmd(m.conn, protocol.TypeProcStop, protocol.ProcActionPayload{ID: p.ID})
	case "d":
		p := v.current()
		if p == nil {
			return m, nil
		}
		if p.Status == "running" {
			m.appendLine("⚠ 进程还在运行，先按 s 停止它，再删除记录")
			return m, nil
		}
		if v.confirm != "d" {
			v.confirm = "d"
			m.appendLine("再次按 d 删除这条记录（只删记录，不动进程）")
			return m, nil
		}
		v.confirm = ""
		return m, sendCmd(m.conn, protocol.TypeProcClean, protocol.ProcActionPayload{ID: p.ID})
	case "x":
		p := v.current()
		if p == nil {
			return m, nil
		}
		if p.Status != "orphaned" {
			m.appendLine("⚠ 只有属主已失联的孤儿进程可以强制终止")
			return m, nil
		}
		if v.confirm != "x" {
			v.confirm = "x"
			m.appendLine("再次按 x 校验启动时间并终止这个孤儿进程（不可撤销）")
			return m, nil
		}
		v.confirm = ""
		return m, sendCmd(m.conn, protocol.TypeProcKill, protocol.ProcActionPayload{ID: p.ID})
	}
	if v.selected < v.offset {
		v.offset = v.selected
	}
	if v.selected >= v.offset+procsPanelHeight {
		v.offset = v.selected - procsPanelHeight + 1
	}
	return m, nil
}

func (m *Model) renderProcsPanel() string {
	v := m.procsPanel
	var b strings.Builder
	running, orphaned := 0, 0
	for _, p := range v.items {
		switch p.Status {
		case "running":
			running++
		case "orphaned":
			orphaned++
		}
	}
	fmt.Fprintf(&b, "▦ 后台进程（%d 条，运行中 %d，孤儿 %d）\n", len(v.items), running, orphaned)
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	end := v.offset + procsPanelHeight
	if end > len(v.items) {
		end = len(v.items)
	}
	for i := v.offset; i < end; i++ {
		b.WriteString(strings.Join(wrapText(procLine(v.items[i], i == v.selected), panelWidth), "\n"))
		b.WriteByte('\n')
	}
	if v.confirm == "d" {
		b.WriteString("再按一次 d 删除记录；其它键取消")
	} else if v.confirm == "x" {
		b.WriteString("再按一次 x 终止进程；其它键取消")
	} else {
		b.WriteString("[↑↓/jk] 选择  [s] 停止  [x] 终止孤儿  [d] 删除记录  [r] 刷新  [esc] 关闭")
	}
	return b.String()
}

// procLine 把一条记录渲染成一行：状态、命令、pid、会话、启动时间。
//
// 命令可能没记下来（旧记录、或登记时属主已崩溃），这时显式写「未知命令」，
// 而不是留空让人以为没有 argv。
func procLine(p protocol.ProcInfo, selected bool) string {
	marker := "  "
	if selected {
		marker = "▶ "
	}
	cmd := strings.Join(p.Argv, " ")
	if cmd == "" {
		cmd = "未知命令"
	}
	parts := []string{marker + procStatusLabel(p.Status), cmd}
	if p.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", p.PID))
	}
	if p.SessionID != "" {
		parts = append(parts, shortID(p.SessionID))
	}
	if p.StartedAt > 0 {
		parts = append(parts, procAge(p.StartedAt))
	}
	if p.StopRequested {
		parts = append(parts, "已请求停止")
	}
	return strings.Join(parts, " · ")
}

func procStatusLabel(status string) string {
	switch status {
	case "running":
		return "[运行中]"
	case "orphaned":
		return "[孤儿]"
	case "exited":
		return "[已退出]"
	case "stopped":
		return "[已停止]"
	default:
		return "[" + status + "]"
	}
}

// procAge 把启动时间转成「3 分钟前」这类相对时间；未来时间按现在算。
func procAge(startedMS int64) string {
	d := time.Since(time.UnixMilli(startedMS))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}
