package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func TestRenderDiffPaging(t *testing.T) {
	diff := "line1\nline2\nline3\nline4\nline5\n"

	lines, hasMore := renderDiff(diff, 0, 2)
	if len(lines) != 2 || lines[0] != "line1" || !hasMore {
		t.Fatalf("首屏 = %v，hasMore = %v", lines, hasMore)
	}

	lines, hasMore = renderDiff(diff, 3, 2)
	if len(lines) != 2 || lines[0] != "line4" || hasMore {
		t.Fatalf("尾屏 = %v，hasMore = %v", lines, hasMore)
	}

	// 视口比内容高：返回全部且没有更多。
	lines, hasMore = renderDiff(diff, 0, 100)
	if len(lines) != 5 || hasMore {
		t.Fatalf("大视口 = %v，hasMore = %v", lines, hasMore)
	}

	// 越界偏移夹到末尾，不能 panic。
	lines, hasMore = renderDiff(diff, 999, 3)
	if len(lines) != 0 || hasMore {
		t.Fatalf("越界偏移 = %v，hasMore = %v", lines, hasMore)
	}
}

func TestApprovalRequestedOpensPanelAndResolvedClosesIt(t *testing.T) {
	m := newModel(t)

	m.handleEvent(eventEnvelope(t, 10, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_1",
		Kind:       domain.ApprovalDiff,
		Path:       "internal/app/app.go",
		Diff:       "@@ -1 +1 @@\n-a\n+b\n",
	}))
	if m.approval == nil {
		t.Fatal("approval.requested 应打开审批面板")
	}
	if m.approval.id != "apr_1" || m.approval.path != "internal/app/app.go" {
		t.Fatalf("面板状态 = %+v", m.approval)
	}
	panel := m.renderApprovalPanel()
	for _, want := range []string{"待审批", "-a", "+b", "[y] 批准一次"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("面板缺少 %q：\n%s", want, panel)
		}
	}

	// 别的审批被裁决不影响当前面板。
	m.handleEvent(eventEnvelope(t, 11, domain.EventApprovalResolved, domain.ApprovalResolvedPayload{
		ApprovalID: "apr_other", Decision: domain.DecisionAllowOnce,
	}))
	if m.approval == nil {
		t.Fatal("无关的裁决不该关掉面板")
	}

	m.handleEvent(eventEnvelope(t, 12, domain.EventApprovalResolved, domain.ApprovalResolvedPayload{
		ApprovalID: "apr_1", Decision: domain.DecisionDeny,
	}))
	if m.approval != nil {
		t.Fatal("对应的裁决应关闭面板")
	}
}

// 断线后 m.conn 会被清空：此时任何 sendCmd 都必须安全地报「已断开」，
// 不能把空指针交给 wsjson.Write 崩掉整个客户端。
func TestSendCmdWithoutConnectionDoesNotPanic(t *testing.T) {
	msg := sendCmd(nil, protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{})()
	if _, ok := msg.(closedMsg); !ok {
		t.Fatalf("nil 连接应返回 closedMsg，实际 %T", msg)
	}
}

// 面板打开时 esc 不退出：审阅 diff 时按错一次不该丢掉整个会话。
//
// 这里走 handleKey 而不是 handleApprovalKey：退出与否由前者的分发顺序决定，
// 只测叶子函数的话，分发改一处就测不出来了。
func TestApprovalPanelIgnoresEsc(t *testing.T) {
	m := newModel(t)
	m.approval = &approvalView{id: "apr_1", path: "a.txt", diff: "+x\n"}

	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		t.Fatal("面板打开时 esc 不该触发任何命令（退出或取消任务都不行）")
	}
	if m.approval == nil {
		t.Fatal("面板不该被 esc 关掉")
	}
}

// 面板打开时 ctrl+r 必须能重连：否则断线后用户只剩 ctrl+c 一条路。
func TestApprovalPanelCtrlRReconnects(t *testing.T) {
	m := newModel(t)
	m.approval = &approvalView{id: "apr_1", path: "a.txt", diff: "+x\n"}

	_, cmd := m.handleApprovalKey("ctrl+r")
	if cmd == nil {
		t.Fatal("ctrl+r 应发起重连")
	}
	if m.approval != nil {
		t.Fatal("重连会建新会话，旧审批面板应清掉")
	}
}

// diff 被截断时面板必须说出来，面板头的 +N −M 也要标成截断版。
func TestApprovalPanelShowsTruncationAndStats(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 30, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_1",
		Kind:       domain.ApprovalDiff,
		Path:       "big.txt",
		Diff:       "@@ -1 +1 @@\n-a\n+b\n",
		Truncated:  true,
	}))
	if m.approval == nil {
		t.Fatal("approval.requested 应打开审批面板")
	}
	if !m.approval.truncated {
		t.Fatal("载荷里的 truncated 应传到面板")
	}
	if m.approval.added != 1 || m.approval.removed != 1 {
		t.Fatalf("改动规模 = +%d -%d，期望 +1 -1", m.approval.added, m.approval.removed)
	}
	panel := m.renderApprovalPanel()
	for _, want := range []string{"（+1 −1，已截断）", "[diff 过长已截断", "[ctrl+r] 重连"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("面板缺少 %q：\n%s", want, panel)
		}
	}
}

func TestApprovalKeysScroll(t *testing.T) {
	m := newModel(t)
	m.approval = &approvalView{
		id:   "apr_1",
		path: "a.txt",
		diff: strings.Repeat("line\n", 100),
	}

	if _, _ = m.handleApprovalKey("down"); m.approval.offset != 1 {
		t.Fatalf("down 后 offset = %d", m.approval.offset)
	}
	m.handleApprovalKey("up")
	m.handleApprovalKey("up")
	if m.approval.offset != 0 {
		t.Fatalf("上翻应夹在 0，实际 %d", m.approval.offset)
	}
	m.handleApprovalKey("pgdown")
	if m.approval.offset != diffPageStep {
		t.Fatalf("pgdown 后 offset = %d", m.approval.offset)
	}
	m.handleApprovalKey("pgup")
	m.handleApprovalKey("pgup")
	if m.approval.offset != 0 {
		t.Fatalf("pgup 应夹在 0，实际 %d", m.approval.offset)
	}

	// 面板打开时未识别的按键被忽略，不会把字符打进输入框。
	m.input = ""
	m.handleApprovalKey("z")
	if m.input != "" {
		t.Fatalf("审阅 diff 时按键不该进输入框，实际 %q", m.input)
	}
	// 走一遍完整按键分发：面板打开时 z 不应落到输入框。
	if _, _ = m.handleKey(tea.KeyPressMsg{Code: 'z', Text: "z"}); m.input != "" {
		t.Fatalf("面板打开时 handleKey 不该把字符写进输入框，实际 %q", m.input)
	}
}

// 断线时面板不能自己消失：审批还在服务端等着，清掉面板等于假装已经处理。
func TestApprovalKeyWithoutConnectionKeepsPanel(t *testing.T) {
	m := newModel(t)
	m.conn = nil
	m.approval = &approvalView{id: "apr_1", path: "a.txt", diff: "+x\n"}

	if _, cmd := m.handleApprovalKey("y"); cmd != nil {
		t.Fatal("没有连接时不该发帧")
	}
	if m.approval == nil {
		t.Fatal("没有连接时面板应保留")
	}
}

func TestProposalEventsAppendLines(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 20, domain.EventProposalApplied, domain.ProposalAppliedPayload{
		ProposalID: "prp_1", Path: "a.txt", NewHash: "h",
	}))
	m.handleEvent(eventEnvelope(t, 21, domain.EventProposalRejected, domain.ProposalRejectedPayload{
		ProposalID: "prp_2", Path: "b.txt", Status: domain.ProposalStale,
	}))

	joined := strings.Join(m.dialog, "\n")
	for _, want := range []string{"已写入 a.txt", "未写入 b.txt（stale）"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("对话缺少 %q：\n%s", want, joined)
		}
	}
}
