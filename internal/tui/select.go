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
	// abs 是这一行在完整内容里的行号（只对 selectable 行有效）：键盘选择
	// 用内容行号做坐标，翻页后仍然成立；固定行填 -1。
	abs int
}

// cursor 是屏幕上的一个单元格坐标：row 为屏幕行号（0 起），col 为显示列（0 起）。
type cursor struct {
	row int
	col int
}

// selectionState 是框选范围。鼠标松开左键后立即清空，不保留历史选区；
// 键盘选择模式则在按 y/enter 复制或 esc 退出时清空。
//
// absolute 区分坐标语义：鼠标用的是屏幕行号（只对当前帧有效），键盘用的是
// 内容行号（相对整段对话，翻页后仍然成立）。
type selectionState struct {
	active   bool
	moved    bool
	absolute bool
	anchor   cursor
	cur      cursor
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
	// 键盘选择模式下鼠标不接管：两套光标同时存在会让人分不清在改哪个。
	if m.selectMode || !m.mouseSelectable() || msg.Button != tea.MouseLeft {
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

// ---------------------------------------------------------------------------
// 键盘选择

// keySelectable 说明能否进入键盘选择模式：与鼠标框选一样只支持全屏——
// 内联模式没有固定视口，既无法把光标滚回可视区，屏幕行号也和终端
// scrollback 对不上。
func (m *Model) keySelectable() bool {
	return m.fullscreen && !m.quitting
}

// enterSelectMode 进入键盘选择：光标停在最后一行行首，因为最常复制的是
// 刚输出的那段。
func (m *Model) enterSelectMode() (tea.Model, tea.Cmd) {
	if !m.keySelectable() {
		m.setNotice("⚠ 内联模式没有固定视口：按 ctrl+f 切全屏后再选", true)
		return m, nil
	}
	if len(m.contentRows) == 0 {
		m.setNotice("⚠ 现在没有可选内容", true)
		return m, nil
	}
	m.selectMode = true
	m.sel = selectionState{}
	m.keyCursor = cursor{row: len(m.contentRows) - 1, col: 0}
	m.ensureCursorVisible()
	return m, nil
}

// exitSelectMode 退出选择模式：光标与半成品选区一起丢掉。
func (m *Model) exitSelectMode() {
	m.selectMode = false
	m.sel = selectionState{}
}

// handleSelectKey 是选择模式下的按键。它是模态的：除了这里的键，其余
// 一律不穿透到输入行——否则按 j/k 会往输入框里塞字母。
func (m *Model) handleSelectKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.exitSelectMode()
		return m, nil
	case "v":
		// 第一次 v 起选，第二次取消选区（光标留着，方便重新框）。
		if m.sel.active {
			m.sel = selectionState{}
		} else {
			m.sel = selectionState{active: true, moved: true, absolute: true, anchor: m.keyCursor, cur: m.keyCursor}
		}
		return m, nil
	case "y", "enter":
		return m.copyKeySelection()
	case "up", "k":
		m.moveKeyCursor(-1, 0)
	case "down", "j":
		m.moveKeyCursor(1, 0)
	case "left", "h":
		m.moveKeyCursor(0, -1)
	case "right", "l":
		m.moveKeyCursor(0, 1)
	case "pgup":
		m.moveKeyCursor(-m.selectPage(), 0)
	case "pgdown":
		m.moveKeyCursor(m.selectPage(), 0)
	case "home":
		m.moveKeyCursor(0, -1<<30)
	case "end":
		m.moveKeyCursor(0, 1<<30)
	default:
		return m, nil
	}
	if m.sel.active {
		m.sel.cur = m.keyCursor
	}
	m.ensureCursorVisible()
	return m, nil
}

// selectPage 是一屏能移动多少行，与翻页保持同一口径。
func (m *Model) selectPage() int {
	page := m.contentHeight - 1
	if page < 3 {
		page = 3
	}
	return page
}

// moveKeyCursor 按行/列移动光标。列移动按字符边界跳，不会停在汉字中间。
func (m *Model) moveKeyCursor(dRow, dCol int) {
	n := len(m.contentRows)
	if n == 0 {
		return
	}
	row := m.keyCursor.row + dRow
	if row < 0 {
		row = 0
	}
	if row >= n {
		row = n - 1
	}
	line := m.contentRows[row].text
	width := displayWidth(line)
	col := m.keyCursor.col
	if dCol < 0 {
		if dCol < -1000 {
			col = 0
		} else {
			col = runeStartBefore(line, col)
		}
	} else if dCol > 0 {
		if dCol > 1000 {
			col = lastRuneStart(line)
		} else {
			col = runeStartAfter(line, col)
		}
	} else if row != m.keyCursor.row {
		// 换行时列不变，但要收进新行的宽度。
		if col > width-1 {
			col = lastRuneStart(line)
		}
	}
	if col < 0 {
		col = 0
	}
	if width > 0 && col > width-1 {
		col = width - 1
	}
	m.keyCursor = cursor{row: row, col: col}
}

// runePositions 返回每个字符的起始列（中文占两列，下一个字符从 +2 开始）。
func runePositions(text string) []int {
	poss := make([]int, 0, len(text))
	col := 0
	for _, r := range text {
		poss = append(poss, col)
		col += runeWidth(r)
	}
	return poss
}

// runeStartBefore 返回 col 之前（含当前字符起点）的字符起始列。
func runeStartBefore(text string, col int) int {
	best := 0
	for _, pos := range runePositions(text) {
		if pos >= col {
			break
		}
		best = pos
	}
	return best
}

// runeStartAfter 返回 col 之后的下一个字符起始列。
func runeStartAfter(text string, col int) int {
	for _, pos := range runePositions(text) {
		if pos > col {
			return pos
		}
	}
	return lastRuneStart(text)
}

// lastRuneStart 返回最后一个字符的起始列（空行返回 0）。
func lastRuneStart(text string) int {
	poss := runePositions(text)
	if len(poss) == 0 {
		return 0
	}
	return poss[len(poss)-1]
}

// ensureCursorVisible 调整滚动位置，让光标留在可视区内。窗口是
// 「内容末尾往前 avail 行」，所以往下翻是把 scroll 减小。
func (m *Model) ensureCursorVisible() {
	n := len(m.contentRows)
	if n == 0 {
		m.scroll = 0
		return
	}
	avail := m.contentHeight
	if avail < 1 {
		avail = 1
	}
	row := m.keyCursor.row
	end := n - m.scroll
	start := end - avail
	if row >= end {
		m.scroll = n - row - 1
	} else if row < start {
		m.scroll = n - row - avail
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
	if m.scroll == 0 {
		m.unseen = 0
	}
}

// copyKeySelection 复制当前选区；没有选区时复制光标所在整行——
// 「挑一行复制」是这个模式最常见的用法，不该逼人先按两次 v。
func (m *Model) copyKeySelection() (tea.Model, tea.Cmd) {
	rows := m.contentRows
	if len(rows) == 0 {
		m.setNotice("⚠ 现在没有可选内容", true)
		return m, nil
	}
	sel := m.sel
	if !sel.active {
		row := m.keyCursor.row
		if row < 0 || row >= len(rows) {
			return m, nil
		}
		sel = selectionState{
			active:   true,
			moved:    true,
			absolute: true,
			anchor:   cursor{row: row, col: 0},
			cur:      cursor{row: row, col: lastRuneStart(rows[row].text)},
		}
	}
	text, lines := extractSelection(rows, sel)
	if strings.TrimSpace(text) == "" {
		m.setNotice("⚠ 选区里没有内容", true)
		return m, nil
	}
	m.exitSelectMode()
	return m, copyCmd(text, lines)
}
