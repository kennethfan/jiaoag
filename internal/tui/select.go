package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// viewLine 是一行已经排好版的屏幕行。
//
//   - text 是去掉 ANSI 的纯文本，复制与高亮都基于它；
//   - 相邻行的 group 相同说明它们来自同一条逻辑行（终端宽度导致的软折行），
//     复制时拼回去而不是插入换行；
//   - selectable 为 false 的行（标签行、状态栏、面板、补白空行）不参与框选。
type viewLine struct {
	text       string
	group      int
	selectable bool
}

// cursor 是屏幕上的一个单元格坐标：row 为屏幕行号（0 起），col 为显示列（0 起）。
type cursor struct {
	row int
	col int
}

// selectionState 是鼠标拖选的范围。松开左键后立即清空，不保留历史选区。
type selectionState struct {
	active bool
	moved  bool
	anchor cursor
	cur    cursor
}

// ordered 返回按阅读顺序排好的两个端点（起点在前）。
func (s selectionState) ordered() (cursor, cursor) {
	a, c := s.anchor, s.cur
	if a.row > c.row || (a.row == c.row && a.col > c.col) {
		return c, a
	}
	return a, c
}

// span 返回第 row 行上被选中的列区间（闭区间），第二个返回值为 false 表示这行没被选。
func (s selectionState) span(row, width int) (int, int, bool) {
	if !s.active || width <= 0 {
		return 0, 0, false
	}
	start, end := s.ordered()
	if row < start.row || row > end.row {
		return 0, 0, false
	}
	from, to := 0, width-1
	if row == start.row {
		from = start.col
	}
	if row == end.row {
		to = end.col
	}
	if to < from {
		return 0, 0, false
	}
	return from, to, true
}

// sliceCells 按显示列切片：from/to 是闭区间，中文等宽字符占两列，
// 直接按 rune 下标切会把一个汉字劈开。
func sliceCells(text string, from, to int) string {
	if to < from {
		return ""
	}
	var b strings.Builder
	col := 0
	kept := false
	for _, r := range text {
		w := runeWidth(r)
		if w == 0 {
			// 组合字符跟随前一个字符：前一个被选中就一起带上。
			if kept {
				b.WriteRune(r)
			}
			continue
		}
		kept = col >= from && col <= to
		if kept {
			b.WriteRune(r)
		}
		col += w
		if col > to {
			break
		}
	}
	return b.String()
}

// highlightCells 给选中区间套反显。styler 关闭（NO_COLOR）时原样返回，
// 靠光标/提示行已经足够说明问题。
func highlightCells(text string, from, to int, enabled bool) string {
	if !enabled || to < from {
		return text
	}
	var b strings.Builder
	col := 0
	on := false
	for _, r := range text {
		w := runeWidth(r)
		if w == 0 {
			b.WriteRune(r)
			continue
		}
		sel := col >= from && col <= to
		if sel != on {
			if sel {
				b.WriteString("\x1b[7m")
			} else {
				b.WriteString("\x1b[0m")
			}
			on = sel
		}
		b.WriteRune(r)
		col += w
	}
	if on {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// extractSelection 把选区还原成文本：同组（软折行）的相邻行直接相接，
// 跨组、跨行则插入换行。返回文本与行数。
func extractSelection(rows []viewLine, sel selectionState) (string, int) {
	start, end := sel.ordered()
	if start.row < 0 {
		start.row = 0
	}
	if end.row >= len(rows) {
		end.row = len(rows) - 1
	}

	var b strings.Builder
	lines := 0
	prevGroup := -2
	for i := start.row; i <= end.row && i < len(rows); i++ {
		row := rows[i]
		if !row.selectable {
			continue
		}
		width := displayWidth(row.text)
		from, to := 0, width-1
		if i == start.row {
			from = start.col
		}
		if i == end.row {
			to = end.col
		}
		if to < from {
			continue
		}
		piece := sliceCells(row.text, from, to)
		if piece == "" {
			continue
		}
		if b.Len() > 0 {
			if !(row.group == prevGroup && prevGroup >= 0) {
				b.WriteByte('\n')
			}
		}
		b.WriteString(piece)
		prevGroup = row.group
		lines++
	}
	return b.String(), lines
}

// selectableRow 判断屏幕第 y 行能否开始框选。
func (m *Model) selectableRow(y int) (int, bool) {
	if y < 0 || y >= len(m.viewRows) || !m.viewRows[y].selectable {
		return 0, false
	}
	return y, true
}

// nearestSelectableRow 把越界的行号收进最近的可选行：拖到状态栏或面板上时
// 选区应停在内容区的边界，而不是突然消失。
func (m *Model) nearestSelectableRow(y int) (int, bool) {
	if len(m.viewRows) == 0 {
		return 0, false
	}
	if y < 0 {
		y = 0
	}
	if y >= len(m.viewRows) {
		y = len(m.viewRows) - 1
	}
	for i := y; i >= 0; i-- {
		if m.viewRows[i].selectable {
			return i, true
		}
	}
	for i := y; i < len(m.viewRows); i++ {
		if m.viewRows[i].selectable {
			return i, true
		}
	}
	return 0, false
}

// clampCol 把鼠标列收进这一行的宽度范围。
func (m *Model) clampCol(row, x int) int {
	width := displayWidth(m.viewRows[row].text)
	if x < 0 {
		return 0
	}
	if x > width-1 {
		if width <= 0 {
			return 0
		}
		return width - 1
	}
	return x
}

// mouseSelectable 说明当前能否用鼠标框选：只有全屏模式下行号才与屏幕一一对应。
func (m *Model) mouseSelectable() bool {
	return m.mouse && m.fullscreen && !m.quitting
}

func (m *Model) handleMousePress(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if !m.mouseSelectable() || msg.Button != tea.MouseLeft {
		return m, nil
	}
	row, ok := m.selectableRow(msg.Y)
	if !ok {
		return m, nil
	}
	at := cursor{row: row, col: m.clampCol(row, msg.X)}
	m.sel = selectionState{active: true, anchor: at, cur: at}
	m.notice = ""
	m.noticeErr = false
	return m, nil
}

func (m *Model) handleMouseDrag(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	if !m.mouseSelectable() || !m.sel.active {
		return m, nil
	}
	row, ok := m.nearestSelectableRow(msg.Y)
	if !ok {
		return m, nil
	}
	col := m.clampCol(row, msg.X)
	if row != m.sel.cur.row || col != m.sel.cur.col {
		m.sel.moved = true
	}
	m.sel.cur = cursor{row: row, col: col}
	return m, nil
}

func (m *Model) handleMouseRelease(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft || !m.sel.active {
		return m, nil
	}
	sel := m.sel
	m.sel = selectionState{}
	if !sel.moved {
		// 单击不复制：误触不该往剪贴板里塞一个字。
		return m, nil
	}
	text, lines := extractSelection(m.viewRows, sel)
	if strings.TrimSpace(text) == "" {
		return m, nil
	}
	return m, copyCmd(text, lines)
}

// handleClipboard 处理复制结果：平台剪贴板失败就退回 OSC 52。
//
// 结果只写状态栏：复制是隔几秒就要做一次的动作，往对话区塞提示会打断阅读，
// 也把刚复制过的那段正文顶走。
func (m *Model) handleClipboard(msg clipboardMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setNotice("⚠ 已用 OSC 52 写入剪贴板（终端可能不支持）", true)
		return m, tea.SetClipboard(msg.text)
	}
	m.setNotice(fmt.Sprintf("✓ 已复制 %d 行 / %d 字符", msg.lines, len([]rune(msg.text))), false)
	return m, nil
}

// setNotice 写一条状态栏临时提示。
func (m *Model) setNotice(text string, isErr bool) {
	m.notice = text
	m.noticeErr = isErr
}
