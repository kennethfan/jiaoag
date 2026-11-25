package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func createdFrame(t *testing.T, id, title string) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(protocol.TypeSessionCreated, protocol.SessionPayload{ID: id, Title: title})
	if err != nil {
		t.Fatalf("构造 session.created: %v", err)
	}
	return env
}

func eventFrame(t *testing.T, sessionID, kind string, payload any) protocol.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码事件载荷: %v", err)
	}
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{Kind: kind, Payload: raw})
	if err != nil {
		t.Fatalf("构造事件帧: %v", err)
	}
	env.SessionID = sessionID
	return env
}

// 多开：新建会话不该顶掉旧的，旧会话留在标签行上继续跑。
func TestSessionCreatedOpensTabInsteadOfReplacing(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "旧会话", "", "")
	m.handleEnvelope(createdFrame(t, "ses_2", "新会话"))

	if len(m.tabs) != 2 {
		t.Fatalf("应有两个标签：%+v", m.tabs)
	}
	if m.sessionID != "ses_2" {
		t.Fatalf("当前会话应为新建的：%q", m.sessionID)
	}
	strip := stripANSI(m.renderTabStrip(120))
	for _, want := range []string{"旧会话", "新会话"} {
		if !strings.Contains(strip, want) {
			t.Fatalf("标签行缺 %q：%s", want, strip)
		}
	}
}

func TestCycleTabsSwitchesSession(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "一", "", "")
	m.openTab("ses_2", "二", "", "")
	m.sessionID = "ses_1"

	_, cmd := m.handleKey(keyMsg("ctrl+n"))
	if cmd == nil {
		t.Fatal("切换标签应重新订阅")
	}
	if m.sessionID != "ses_2" {
		t.Fatalf("ctrl+n 应切到下一条：%q", m.sessionID)
	}
	if m.sessionTitle != "二" {
		t.Fatalf("标题没跟着切：%q", m.sessionTitle)
	}

	_, _ = m.handleKey(keyMsg("ctrl+p"))
	if m.sessionID != "ses_1" {
		t.Fatalf("ctrl+p 应切回上一条：%q", m.sessionID)
	}
}

// 后台会话的事件只更新标签徽标，不污染当前对话区。
func TestBackgroundEventUpdatesTabBadge(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "后台", "", "")
	m.openTab("ses_2", "当前", "", "")
	m.sessionID = "ses_2"
	before := len(m.dialog)

	m.handleEnvelope(eventFrame(t, "ses_1", string(domain.EventTaskStatus),
		domain.TaskStatusPayload{To: domain.TaskRunning}))

	if len(m.dialog) != before {
		t.Fatalf("后台事件不该写入对话区：%v", m.dialog[before:])
	}
	tab := m.tabs[m.tabIndex("ses_1")]
	if tab.Status != string(domain.TaskRunning) {
		t.Fatalf("标签状态没更新：%+v", tab)
	}
	if strip := stripANSI(m.renderTabStrip(120)); !strings.Contains(strip, "●") {
		t.Fatalf("运行中应打点：%s", strip)
	}
}

func TestBackgroundApprovalMarksTab(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "后台", "", "")
	m.openTab("ses_2", "当前", "", "")
	m.sessionID = "ses_2"

	m.handleEnvelope(eventFrame(t, "ses_1", string(domain.EventApprovalRequested),
		domain.ApprovalRequestedPayload{ApprovalID: "apr_1", Kind: domain.ApprovalDiff, Path: "a.txt"}))

	tab := m.tabs[m.tabIndex("ses_1")]
	if tab.Pending != 1 {
		t.Fatalf("待审批没记上：%+v", tab)
	}
	if strip := stripANSI(m.renderTabStrip(120)); !strings.Contains(strip, "!") {
		t.Fatalf("待审批应有感叹号：%s", strip)
	}
}

func TestCloseTabSwitchesToNeighbour(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "一", "", "")
	m.openTab("ses_2", "二", "", "")
	m.sessionID = "ses_2"

	_, cmd := m.handleKey(keyMsg("ctrl+w"))
	if cmd == nil {
		t.Fatal("关标签要退订（发 event.unsubscribe）")
	}
	if len(m.tabs) != 1 || m.tabs[0].ID != "ses_1" {
		t.Fatalf("应只剩 ses_1：%+v", m.tabs)
	}
	if m.sessionID != "ses_1" {
		t.Fatalf("应切到相邻标签：%q", m.sessionID)
	}
}

func TestCloseLastTabIsRefused(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "一", "", "")
	m.sessionID = "ses_1"

	_, cmd := m.handleKey(keyMsg("ctrl+w"))
	if cmd != nil {
		t.Fatal("最后一条不该关")
	}
	if !dialogContains(m, "至少保留一个") {
		t.Fatalf("缺少提示：%v", m.dialog)
	}
}

// 单会话时不画标签行：多一行只会挤掉内容。
func TestTabStripHiddenForSingleSession(t *testing.T) {
	m := newModel(t)
	m.openTab("ses_1", "一", "", "")
	if strip := m.renderTabStrip(120); strip != "" {
		t.Fatalf("单会话不该有标签行：%q", strip)
	}
}

func TestNewCommandOpensSecondTab(t *testing.T) {
	m := newModel(t)

	// 第一条会话（启动时自动创建）。
	first := createdFrame(t, "ses_first", "第一条")
	m.handleEnvelope(first)
	if m.sessionID != "ses_first" {
		t.Fatalf("应先进入第一条：%q", m.sessionID)
	}
	m.appendLine("旧会话的历史")

	m.input = "/new"
	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("/new 应发 session.create")
	}

	// 服务端回第二条。
	second := createdFrame(t, "ses_second", "第二条")
	m.handleEnvelope(second)

	if m.sessionID != "ses_second" {
		t.Fatalf("应切到新会话：%q", m.sessionID)
	}
	if len(m.tabs) != 2 {
		t.Fatalf("应有两条标签：%d", len(m.tabs))
	}
	if m.tabs[0].ID != "ses_first" || m.tabs[1].ID != "ses_second" {
		t.Fatalf("标签顺序不对：%+v", m.tabs)
	}
	// 新会话的对话区不能带着旧会话的历史。
	if dialogContains(m, "旧会话的历史") {
		t.Fatalf("新会话不该继承旧对话：%v", m.dialog)
	}
	strip := stripANSI(m.renderTabStrip(120))
	if !strings.Contains(strip, "第一条") || !strings.Contains(strip, "第二条") {
		t.Fatalf("标签行缺内容：%s", strip)
	}
}

func TestTabStripRendersAtTop(t *testing.T) {
	m := newModel(t)
	m.width, m.height = 80, 20
	m.fullscreen = true
	m.openTab("ses_1", "第一条", "", "")
	m.openTab("ses_2", "第二条", "", "")
	m.sessionID = "ses_2"
	for i := 0; i < 40; i++ {
		m.dialog = append(m.dialog, fmt.Sprintf("历史 %d", i))
	}

	lines := strings.Split(m.View().Content, "\n")
	if len(lines) != 20 {
		t.Fatalf("帧高应恒为 20：%d", len(lines))
	}
	first := stripANSI(lines[0])
	if !strings.Contains(first, "第一条") || !strings.Contains(first, "第二条") {
		t.Fatalf("标签行应在第一行：%q", first)
	}
	// 状态栏仍然钉在最后一行。
	if !strings.Contains(stripANSI(lines[19]), "已连接") && !strings.Contains(stripANSI(lines[19]), "未连接") {
		t.Fatalf("末行应是状态栏：%q", stripANSI(lines[19]))
	}
}

func TestTabStripKeepsCurrentVisibleWhenCrowded(t *testing.T) {
	m := newModel(t)
	for i := 1; i <= 8; i++ {
		m.openTab(fmt.Sprintf("ses_%d", i), fmt.Sprintf("第%d条会话", i), "", "")
	}
	m.sessionID = "ses_8" // 最后一条：前面都得折叠

	strip := stripANSI(m.renderTabStrip(60))
	if displayWidth(strip) > 60 {
		t.Fatalf("标签行超宽：%d %q", displayWidth(strip), strip)
	}
	if !strings.Contains(strip, "第8条会话") {
		t.Fatalf("当前标签必须可见：%q", strip)
	}
	if !strings.Contains(strip, "…") {
		t.Fatalf("放不下的部分要有省略标记：%q", strip)
	}

	// 切到中间那条：窗口要跟着移到中间，仍然包含当前标签。
	m.sessionID = "ses_4"
	strip = stripANSI(m.renderTabStrip(60))
	if !strings.Contains(strip, "第4条会话") {
		t.Fatalf("切到中间后当前标签应可见：%q", strip)
	}
	if !strings.HasPrefix(strip, "…") && !strings.HasSuffix(strip, "…") && !strings.Contains(strip, "+") {
		t.Fatalf("应标出还有未显示的标签：%q", strip)
	}
}

func TestTabClearsRunningMarkerAfterTaskCompletes(t *testing.T) {
	m := newModel(t)
	m.width, m.height = 80, 20
	m.openTab("ses_1", "甲", "", "")
	m.sessionID = "ses_1"

	m.handleEvent(taskStatusFrame(t, "ses_1", "tsk_1", domain.TaskStatusPayload{From: domain.TaskRunning, To: domain.TaskRunning}))
	if got := m.tabs[0].Status; got != string(domain.TaskRunning) {
		t.Fatalf("进行中应标在标签上：%q", got)
	}

	m.handleEvent(taskStatusFrame(t, "ses_1", "tsk_1", domain.TaskStatusPayload{From: domain.TaskRunning, To: domain.TaskCompleted}))
	if got := m.tabs[0].Status; got != string(domain.TaskCompleted) {
		t.Fatalf("终态应回写标签：%q", got)
	}
	if marker := tabMarker(m.tabs[0]); marker == "●" {
		t.Fatalf("完成后不该还显示运行中标记")
	}
}

// taskStatusFrame 把任务 id 写进信封：TUI 靠它区分「哪个任务」的状态变化。
func taskStatusFrame(t *testing.T, sessionID, taskID string, payload any) protocol.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码事件载荷: %v", err)
	}
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind: string(domain.EventTaskStatus), Payload: raw, TaskID: taskID,
	})
	if err != nil {
		t.Fatalf("构造事件帧: %v", err)
	}
	env.SessionID = sessionID
	return env
}
