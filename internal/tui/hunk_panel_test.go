package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/patch"
)

// twoHunkApproval 造一个带两个块、默认全选的面板状态。
func twoHunkApproval(t *testing.T, m *Model) *approvalView {
	t.Helper()
	diff := "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n@@ -10,3 +10,3 @@\n j\n-k\n+K\n l\n"
	hunks, err := patch.ParseHunks(diff)
	if err != nil {
		t.Fatalf("解析 diff: %v", err)
	}
	if len(hunks) != 2 {
		t.Fatalf("块数 = %d", len(hunks))
	}
	v := &approvalView{
		id:      "apr_1",
		kind:    string(domain.ApprovalDiff),
		path:    "notes.txt",
		diff:    diff,
		hunks:   hunks,
		hunkSel: []bool{true, true},
	}
	m.approval = v
	return v
}

func TestHunkSelectModeTogglesAndNavigates(t *testing.T) {
	m := newModel(t)
	v := twoHunkApproval(t, m)

	m.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	if !v.selectMode {
		t.Fatal("v 应进入逐块模式")
	}

	// 默认全选，space 取消当前块。
	m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if v.hunkSel[0] {
		t.Fatal("space 应取消第一块")
	}

	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if v.hunkCursor != 1 {
		t.Fatalf("j 后 cursor = %d", v.hunkCursor)
	}
	m.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	if v.hunkSel[1] {
		t.Fatal("space 应取消第二块")
	}
	if selectedHunks(v.hunkSel) != 0 {
		t.Fatalf("勾选数 = %d", selectedHunks(v.hunkSel))
	}

	// a 全选、n 全不选。
	m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if selectedHunks(v.hunkSel) != 2 {
		t.Fatalf("a 后勾选数 = %d", selectedHunks(v.hunkSel))
	}
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if selectedHunks(v.hunkSel) != 0 {
		t.Fatalf("n 后勾选数 = %d", selectedHunks(v.hunkSel))
	}
}

func TestHunkSelectRequiresSelection(t *testing.T) {
	m := newModel(t)
	twoHunkApproval(t, m)
	m.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})

	before := len(m.dialog)
	m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.approval == nil {
		t.Fatal("一块都没勾时 y 不该提交")
	}
	if len(m.dialog) == before {
		t.Fatal("应该提示至少勾选一块")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "至少") {
		t.Fatalf("提示文案 = %v", m.dialog)
	}
}

func TestHunkSelectEscReturnsToFullView(t *testing.T) {
	m := newModel(t)
	v := twoHunkApproval(t, m)
	m.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if v.selectMode {
		t.Fatal("esc 应退出逐块模式")
	}
	if v.hunkCursor != 0 {
		t.Fatalf("esc 后 cursor = %d", v.hunkCursor)
	}
	if m.approval == nil {
		t.Fatal("退出逐块模式不该丢弃审批面板")
	}
}

func TestApprovalResolvePayloadCarriesPartialHunks(t *testing.T) {
	m := newModel(t)
	v := twoHunkApproval(t, m)

	// 全选：不带 hunks，服务端走整份写入。
	if p := m.approvalResolvePayload(v, domain.DecisionAllowOnce); p.Hunks != nil {
		t.Fatalf("全选时 hunks = %v", p.Hunks)
	}

	// 只留第二块：带上 0 起下标。
	v.hunkSel[0] = false
	p := m.approvalResolvePayload(v, domain.DecisionAllowOnce)
	if len(p.Hunks) != 1 || p.Hunks[0] != 1 {
		t.Fatalf("部分批准 hunks = %v", p.Hunks)
	}
	if p.ApprovalID != "apr_1" || p.Decision != string(domain.DecisionAllowOnce) {
		t.Fatalf("载荷 = %+v", p)
	}
}

func TestRenderHunkSelectPanelShowsHeadersAndBody(t *testing.T) {
	m := newModel(t)
	v := twoHunkApproval(t, m)
	v.hunkSel[1] = false
	v.selectMode = true

	panel := m.renderHunkSelectPanel()
	for _, want := range []string{"逐块审批 notes.txt", "已选 1/2 块", "[✓]", "[ ]", "+1 −1", "-b", "[space] 勾选", "[esc] 返回全量视图"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("面板缺少 %q：\n%s", want, panel)
		}
	}
}

func TestDiffPanelOffersHunkModeOnlyForMultipleHunks(t *testing.T) {
	m := newModel(t)
	single := &approvalView{id: "apr_s", kind: string(domain.ApprovalDiff), path: "a.txt", diff: "@@ -1 +1 @@\n-a\n+b\n"}
	m.approval = single
	if strings.Contains(m.renderApprovalPanel(), "[v] 逐块") {
		t.Fatal("单块 diff 不该提示逐块")
	}

	twoHunkApproval(t, m)
	if !strings.Contains(m.renderApprovalPanel(), "[v] 逐块") {
		t.Fatal("多块 diff 应提示 [v] 逐块")
	}
}
