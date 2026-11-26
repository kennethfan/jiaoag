package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestSliceCellsCountsWideRunes(t *testing.T) {
	text := "中文abc" // 中 0-1，文 2-3，a 4，b 5，c 6
	cases := []struct {
		from, to int
		want     string
	}{
		{0, 1, "中"},
		{2, 2, "文"},
		{4, 6, "abc"},
		{1, 4, "文a"},
		{7, 9, ""},
	}
	for _, c := range cases {
		if got := sliceCells(text, c.from, c.to); got != c.want {
			t.Errorf("sliceCells(%q, %d, %d) = %q，想要 %q", text, c.from, c.to, got, c.want)
		}
	}
}

func TestExtractSelectionJoinsSoftWrappedRows(t *testing.T) {
	rows := []viewLine{
		{text: "alpha be", group: 0, selectable: true},
		{text: "ta", group: 0, selectable: true},
		{text: "second", group: 1, selectable: true},
		{text: "› 输入行", group: -1},
	}
	sel := selectionState{
		active: true,
		anchor: cursor{row: 0, col: 0},
		cur:    cursor{row: 1, col: 1},
	}
	text, lines := extractSelection(rows, sel)
	if text != "alpha beta" {
		t.Fatalf("软折行应该拼回同一行，得到 %q", text)
	}
	if lines != 2 {
		t.Fatalf("参与复制的屏幕行应为 2，得到 %d", lines)
	}

	// 跨逻辑行要保留换行；状态栏那种不可选行直接跳过。
	sel.cur = cursor{row: 3, col: 100}
	text, _ = extractSelection(rows, sel)
	if text != "alpha beta\nsecond" {
		t.Fatalf("跨行结果 = %q", text)
	}
}

func TestExtractSelectionRespectsColumns(t *testing.T) {
	rows := []viewLine{{text: "hello world", group: 0, selectable: true}}
	sel := selectionState{active: true, anchor: cursor{row: 0, col: 6}, cur: cursor{row: 0, col: 10}}
	if text, _ := extractSelection(rows, sel); text != "world" {
		t.Fatalf("按列切出 %q，想要 world", text)
	}
}

func TestMouseDragCopiesSelection(t *testing.T) {
	orig := writeClipboard
	var copied string
	writeClipboard = func(text string) error {
		copied = text
		return nil
	}
	t.Cleanup(func() { writeClipboard = orig })

	m := newModel(t)
	m.fullscreen = true
	m.mouse = true
	m.width, m.height = 40, 12
	m.dialog = nil
	m.appendLine("alpha beta gamma")
	m.View()

	row := -1
	for i, line := range m.viewRows {
		if line.selectable && strings.Contains(line.text, "alpha beta") {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("没在渲染结果里找到目标行：%+v", m.viewRows)
	}

	if _, cmd := m.Update(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft}); cmd != nil {
		t.Fatalf("按下左键不该产生命令")
	}
	m.Update(tea.MouseMotionMsg{X: 4, Y: row, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: 4, Y: row, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatalf("松开左键应该触发复制")
	}
	msg, ok := cmd().(clipboardMsg)
	if !ok {
		t.Fatalf("复制命令返回了 %T", cmd())
	}
	if msg.text != "alpha" || msg.lines != 1 {
		t.Fatalf("复制内容 = %q（%d 行），想要 alpha", msg.text, msg.lines)
	}
	if copied != "alpha" {
		t.Fatalf("剪贴板收到 %q", copied)
	}
	if m.sel.active {
		t.Fatalf("松开后选区应该清空")
	}

	m.Update(msg)
	// 结果只进状态栏：对话区不该被「已复制」这种一次性反馈占掉一行。
	if !strings.Contains(m.notice, "已复制 1 行 / 5 字符") {
		t.Fatalf("状态栏提示缺失：%q", m.notice)
	}
	if strings.Contains(strings.Join(m.dialog, "\n"), "已复制") {
		t.Fatalf("复制结果不该写进对话区：%v", m.dialog)
	}
	bar := stripANSI(m.renderStatusBar(120, false))
	if !strings.Contains(bar, "已复制 1 行 / 5 字符") {
		t.Fatalf("状态栏没渲染提示：%q", bar)
	}

	// 下一次按键就清掉，避免过期信息留在底部。
	m.input = ""
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.notice != "" {
		t.Fatalf("按键后提示应清除，仍是 %q", m.notice)
	}
}

func TestMouseClickWithoutDragCopiesNothing(t *testing.T) {
	m := newModel(t)
	m.fullscreen = true
	m.mouse = true
	m.width, m.height = 40, 12
	m.dialog = nil
	m.appendLine("hello world")
	m.View()

	row := 0
	for i, line := range m.viewRows {
		if line.selectable {
			row = i
			break
		}
	}
	m.Update(tea.MouseClickMsg{X: 2, Y: row, Button: tea.MouseLeft})
	if _, cmd := m.Update(tea.MouseReleaseMsg{X: 2, Y: row, Button: tea.MouseLeft}); cmd != nil {
		t.Fatalf("单击不该复制")
	}
}

func TestMouseSelectionDisabledInline(t *testing.T) {
	m := newModel(t)
	m.fullscreen = false
	m.mouse = true
	m.appendLine("hello")
	m.View()

	if len(m.viewRows) > 0 && m.viewRows[0].selectable {
		m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
		if m.sel.active {
			t.Fatalf("内联模式下行号与屏幕不对应，不应开始框选")
		}
	}
}

func TestClipboardFallbackMentionsOSC52(t *testing.T) {
	m := newModel(t)
	m.dialog = nil
	if _, cmd := m.handleClipboard(clipboardMsg{text: "x", lines: 1, err: errors.New("pbcopy 缺失")}); cmd == nil {
		t.Fatalf("平台剪贴板失败时应退回 OSC 52")
	}
	if !strings.Contains(m.notice, "OSC 52") || !m.noticeErr {
		t.Fatalf("没在状态栏说明回落方式：%q err=%v", m.notice, m.noticeErr)
	}
}

func TestHighlightCellsMarksOnlySelection(t *testing.T) {
	got := highlightCells("abcdef", 1, 3, true)
	if !strings.HasPrefix(got, "a\x1b[7mbcd\x1b[0m") {
		t.Fatalf("高亮包裹不正确：%q", got)
	}
	if plain := stripANSI(got); plain != "abcdef" {
		t.Fatalf("高亮改变了文字：%q", plain)
	}
	if off := highlightCells("abcdef", 1, 3, false); off != "abcdef" {
		t.Fatalf("关闭样式时应原样返回：%q", off)
	}
}
