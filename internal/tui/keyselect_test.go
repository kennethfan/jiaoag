package tui

import (
	"strings"
	"testing"
)

// selectModel 造一个全屏、已有几行内容、光标停稳的模型。
func selectModel(t *testing.T, lines ...string) *Model {
	t.Helper()
	m := newModel(t)
	m.width = 80
	m.height = 20
	m.fullscreen = true
	m.dialog = append([]string(nil), lines...)
	m.View() // 填 contentRows：选择模式的坐标来自最近一帧
	return m
}

// captureClipboard 替换平台写入，返回一个读回写入内容的函数。
func captureClipboard(t *testing.T) func() string {
	t.Helper()
	prev := writeClipboard
	var got string
	writeClipboard = func(text string) error {
		got = text
		return nil
	}
	t.Cleanup(func() { writeClipboard = prev })
	return func() string { return got }
}

func TestCtrlYEntersSelectModeOnlyInFullscreen(t *testing.T) {
	m := selectModel(t, "alpha", "beta")
	m.fullscreen = false
	m.Update(keyMsg("ctrl+y"))
	if m.selectMode {
		t.Fatal("内联模式不该进入选择模式")
	}
	if !strings.Contains(m.notice, "ctrl+f") {
		t.Fatalf("内联模式应提示切全屏，实际 %q", m.notice)
	}

	m.fullscreen = true
	m.Update(keyMsg("ctrl+y"))
	if !m.selectMode {
		t.Fatal("全屏模式应进入选择模式")
	}
	if m.keyCursor.row != len(m.contentRows)-1 {
		t.Fatalf("光标应停在最后一行，实际 row=%d", m.keyCursor.row)
	}
}

func TestSelectModeKeysDoNotType(t *testing.T) {
	m := selectModel(t, "alpha", "beta", "gamma")
	m.Update(keyMsg("ctrl+y"))
	before := m.input
	for _, k := range []string{"k", "h", "j", "l"} {
		m.Update(keyMsg(k))
	}
	if m.input != before {
		t.Fatalf("选择模式下的按键不该写进输入行，实际 %q", m.input)
	}
	if m.keyCursor.row != len(m.contentRows)-1 {
		t.Fatalf("上下移动后应回到最后一行，实际 row=%d", m.keyCursor.row)
	}
}

func TestKeyboardSelectionCopiesRange(t *testing.T) {
	m := selectModel(t, "alpha", "beta", "gamma")
	got := captureClipboard(t)

	m.Update(keyMsg("ctrl+y"))
	m.Update(keyMsg("k")) // beta
	m.Update(keyMsg("k")) // alpha
	m.Update(keyMsg("v"))
	m.Update(keyMsg("j")) // beta
	m.Update(keyMsg("end"))
	_, cmd := m.Update(keyMsg("y"))
	if cmd == nil {
		t.Fatal("复制应返回一个 cmd")
	}
	// copyCmd 是同步执行的，直接跑拿到结果再交给模型。
	msg, ok := cmd().(clipboardMsg)
	if !ok {
		t.Fatalf("期望 clipboardMsg，实际 %T", msg)
	}
	m.Update(msg)
	if text := got(); text != "alpha\nbeta" {
		t.Fatalf("复制内容 = %q", text)
	}
	if msg.lines != 2 {
		t.Fatalf("行数 = %d", msg.lines)
	}
	if m.selectMode || m.sel.active {
		t.Fatal("复制后应退出选择模式")
	}
	if !strings.Contains(m.notice, "已复制") {
		t.Fatalf("状态栏应有复制提示，实际 %q", m.notice)
	}
}

func TestCopyWithoutSelectionCopiesWholeLine(t *testing.T) {
	m := selectModel(t, "alpha", "beta")
	got := captureClipboard(t)
	m.Update(keyMsg("ctrl+y"))
	_, cmd := m.Update(keyMsg("y"))
	if cmd == nil {
		t.Fatal("复制应返回一个 cmd")
	}
	m.Update(cmd())
	if text := got(); text != "beta" {
		t.Fatalf("无选区时应复制整行，实际 %q", text)
	}
}

func TestEscLeavesSelectMode(t *testing.T) {
	m := selectModel(t, "alpha")
	m.Update(keyMsg("ctrl+y"))
	m.Update(keyMsg("v"))
	m.Update(keyMsg("esc"))
	if m.selectMode || m.sel.active {
		t.Fatal("esc 应退出选择模式并清掉选区")
	}
}

func TestSelectModeScrollsToKeepCursorVisible(t *testing.T) {
	lines := make([]string, 80)
	for i := range lines {
		lines[i] = "line"
	}
	m := selectModel(t, lines...)
	m.Update(keyMsg("ctrl+y"))
	if m.scroll != 0 {
		t.Fatalf("光标在末行时不该滚动，实际 %d", m.scroll)
	}
	for i := 0; i < 5; i++ {
		m.Update(keyMsg("pgup"))
	}
	if m.scroll == 0 {
		t.Fatal("往上翻页应改变滚动位置")
	}
	// 光标必须落在可视窗口内：窗口是 [len-scroll-avail, len-scroll)。
	end := len(m.contentRows) - m.scroll
	start := end - m.contentHeight
	if m.keyCursor.row < start || m.keyCursor.row >= end {
		t.Fatalf("光标 row=%d 不在可视区 [%d,%d)", m.keyCursor.row, start, end)
	}
}
