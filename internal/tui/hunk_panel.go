package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// hunkPanelHeight 是块列表一次显示多少行；超出的块靠 hunkCursor 滚动。
const hunkPanelHeight = 12

// handleHunkKey 处理逐块审批模式下的按键。
//
// 这个模式是独立的：普通面板上 y/a/n 是整份裁决，进到这里以后 space 只改勾选，
// y 才真正提交。误按一个键最坏只是多勾/少勾一块，不会直接落盘。
func (m *Model) handleHunkKey(key string) (tea.Model, tea.Cmd) {
	v := m.approval
	switch key {
	case "esc":
		v.selectMode = false
		v.hunkCursor = 0
	case "j", "down":
		if v.hunkCursor < len(v.hunks)-1 {
			v.hunkCursor++
		}
	case "k", "up":
		if v.hunkCursor > 0 {
			v.hunkCursor--
		}
	case " ", "space":
		if v.hunkCursor < len(v.hunkSel) {
			v.hunkSel[v.hunkCursor] = !v.hunkSel[v.hunkCursor]
		}
	case "a":
		for i := range v.hunkSel {
			v.hunkSel[i] = true
		}
	case "n":
		for i := range v.hunkSel {
			v.hunkSel[i] = false
		}
	case "y":
		if selectedHunks(v.hunkSel) == 0 {
			m.appendLine("⚠ 至少要勾选一块；[esc] 返回全量视图")
			return m, nil
		}
		return m.resolveApproval(domain.DecisionAllowOnce)
	}
	return m, nil
}

func selectedHunks(sel []bool) int {
	n := 0
	for _, on := range sel {
		if on {
			n++
		}
	}
	return n
}

// partialHunks 返回需要随审批一起发出去的块下标：没勾满时才带。
// 全选等价于整份应用，发空列表让服务端走原路径。
func partialHunks(v *approvalView) []int {
	if v == nil || len(v.hunkSel) < 2 {
		return nil
	}
	if selectedHunks(v.hunkSel) == len(v.hunkSel) {
		return nil
	}
	out := make([]int, 0, len(v.hunkSel))
	for i, on := range v.hunkSel {
		if on {
			out = append(out, i)
		}
	}
	return out
}

// joinHunkNumbers 把 0 起的下标转成用户看得懂的 1 起编号。
func joinHunkNumbers(hunks []int) string {
	parts := make([]string, 0, len(hunks))
	for _, idx := range hunks {
		parts = append(parts, fmt.Sprintf("%d", idx+1))
	}
	return strings.Join(parts, "、")
}

// renderHunkSelectPanel 渲染块列表 + 当前块正文。
func (m *Model) renderHunkSelectPanel() string {
	v := m.approval
	var b strings.Builder
	fmt.Fprintf(&b, "✎ 逐块审批 %s（已选 %d/%d 块）\n",
		v.path, selectedHunks(v.hunkSel), len(v.hunks))
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')

	start := 0
	if v.hunkCursor >= hunkPanelHeight {
		start = v.hunkCursor - hunkPanelHeight + 1
	}
	for i := start; i < len(v.hunks) && i < start+hunkPanelHeight; i++ {
		mark := "[ ]"
		if v.hunkSel[i] {
			mark = "[✓]"
		}
		pointer := "  "
		if i == v.hunkCursor {
			pointer = "▶ "
		}
		h := v.hunks[i]
		fmt.Fprintf(&b, "%s%s %s  +%d −%d\n", pointer, mark, h.Header, h.Added, h.Removed)
	}
	if len(v.hunks) > hunkPanelHeight {
		fmt.Fprintf(&b, "… 共 %d 块，[j/k] 移动\n", len(v.hunks))
	}

	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	cur := v.hunks[v.hunkCursor]
	for _, line := range cur.Lines {
		b.WriteString("  " + clipLine(line, panelWidth-4) + "\n")
	}
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	b.WriteString("[space] 勾选  [j/k] 换块  [a] 全选  [n] 全不选  [y] 应用选中\n")
	b.WriteString("[esc] 返回全量视图  [ctrl+c] 退出\n")
	return b.String()
}

// clipLine 按 rune 截断，避免一行超长把面板撑乱。
func clipLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width]) + "…"
}
