package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func sessionsFrame(t *testing.T, items ...protocol.SessionSummaryPayload) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(protocol.TypeSessionManageResult, protocol.SessionManageResultPayload{Sessions: items})
	if err != nil {
		t.Fatalf("构造会话列表帧: %v", err)
	}
	return env
}

func openSessionsPanel(t *testing.T, m *Model, items ...protocol.SessionSummaryPayload) {
	t.Helper()
	m.handleEnvelope(sessionsFrame(t, items...))
	if m.sessionsPanel == nil {
		t.Fatal("会话面板没打开")
	}
}

func dialogContains(m *Model, want string) bool {
	for _, line := range m.dialog {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func TestSessionsCommandAsksForList(t *testing.T) {
	m := newModel(t)
	m.input = "/sessions"
	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("应发出 session.manage")
	}
	if m.input != "" {
		t.Fatalf("输入行应被清空，得到 %q", m.input)
	}
	if !dialogContains(m, "正在获取会话列表") {
		t.Fatalf("缺少提示行：%v", m.dialog)
	}
}

func TestSessionsResultOpensPanelSelectingCurrent(t *testing.T) {
	m := newModel(t) // sessionID = ses_1
	openSessionsPanel(t, m,
		protocol.SessionSummaryPayload{ID: "ses_2", Title: "旧的", Tasks: 3, Model: "m", Provider: "p"},
		protocol.SessionSummaryPayload{ID: "ses_1", Title: "当前这条", Tasks: 1, Model: "m", Provider: "p"},
	)
	if m.sessionsPanel.selected != 1 {
		t.Fatalf("应选中当前会话，selected=%d", m.sessionsPanel.selected)
	}
	view := m.renderSessionsPanel()
	for _, want := range []string{"2 条", "旧的", "当前这条 (当前)", "3 任务", "[t] 改名", "[d] 删除"} {
		if !strings.Contains(view, want) {
			t.Fatalf("面板缺少 %q：\n%s", want, view)
		}
	}
}

func TestSessionsPanelEnterSwitchesSession(t *testing.T) {
	m := newModel(t)
	openSessionsPanel(t, m, protocol.SessionSummaryPayload{
		ID: "ses_9", Title: "另一条", Provider: "agnes", Model: "agnes-3.0-flash",
		Workspace: m.opts.Workspace,
	})
	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("切换会话应先退订再订阅")
	}
	if m.sessionID != "ses_9" {
		t.Fatalf("sessionID = %q", m.sessionID)
	}
	if m.sessionTitle != "另一条" {
		t.Fatalf("sessionTitle = %q", m.sessionTitle)
	}
	if m.sessionsPanel != nil {
		t.Fatal("切换后面板应关闭")
	}
	if !dialogContains(m, "切换到会话") {
		t.Fatalf("缺少切换提示：%v", m.dialog)
	}
}

func TestSessionsPanelRefusesSwitchWithActiveTask(t *testing.T) {
	m := newModel(t)
	openSessionsPanel(t, m, protocol.SessionSummaryPayload{ID: "ses_busy", ActiveTasks: 1})
	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("有未结束任务时不该切走")
	}
	if m.sessionID != "ses_1" {
		t.Fatalf("sessionID 不该变：%q", m.sessionID)
	}
	if !dialogContains(m, "没结束") {
		t.Fatalf("缺少拒绝提示：%v", m.dialog)
	}
}

func TestSessionsPanelRenamePrefillsInput(t *testing.T) {
	m := newModel(t)
	openSessionsPanel(t, m, protocol.SessionSummaryPayload{ID: "ses_9", Title: "旧标题"})

	m.handleKey(keyMsg("t"))
	if m.sessionsPanel != nil {
		t.Fatal("改名时面板应关闭，把位置让给输入行")
	}
	if m.renamingSession != "ses_9" {
		t.Fatalf("renamingSession = %q", m.renamingSession)
	}
	if m.input != "旧标题" {
		t.Fatalf("输入行应预填当前标题，得到 %q", m.input)
	}

	// 回车保存：清掉改名态，输入行也清空。
	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("应发出 session.rename")
	}
	if m.renamingSession != "" || m.input != "" {
		t.Fatalf("改名结束后状态没清：%q %q", m.renamingSession, m.input)
	}
}

func TestSessionsPanelRenameEscapeCancels(t *testing.T) {
	m := newModel(t)
	openSessionsPanel(t, m, protocol.SessionSummaryPayload{ID: "ses_9", Title: "旧标题"})
	m.handleKey(keyMsg("t"))

	_, cmd := m.handleKey(keyMsg("esc"))
	if cmd != nil {
		t.Fatal("esc 不该发任何帧")
	}
	if m.renamingSession != "" || m.input != "" {
		t.Fatalf("取消后状态没清：%q %q", m.renamingSession, m.input)
	}
	if !dialogContains(m, "已取消改名") {
		t.Fatalf("缺少取消提示：%v", m.dialog)
	}
}

func TestSessionsPanelDeleteNeedsTwoPresses(t *testing.T) {
	m := newModel(t)
	openSessionsPanel(t, m, protocol.SessionSummaryPayload{ID: "ses_9", Title: "待删"})

	_, cmd := m.handleKey(keyMsg("d"))
	if cmd != nil {
		t.Fatal("第一次按 d 只该提醒")
	}
	if m.sessionsPanel.confirmID != "ses_9" {
		t.Fatalf("confirmID = %q", m.sessionsPanel.confirmID)
	}
	if !dialogContains(m, "永久删除") {
		t.Fatalf("缺少提醒：%v", m.dialog)
	}

	_, cmd = m.handleKey(keyMsg("d"))
	if cmd == nil {
		t.Fatal("第二次按 d 应发 session.delete")
	}
	if m.sessionsPanel.confirmID != "" {
		t.Fatal("确认后应清掉 confirmID")
	}
}

func TestSessionsPanelRefusesDeleteWithActiveTask(t *testing.T) {
	m := newModel(t)
	openSessionsPanel(t, m, protocol.SessionSummaryPayload{ID: "ses_busy", ActiveTasks: 2})
	for i := 0; i < 2; i++ {
		if _, cmd := m.handleKey(keyMsg("d")); cmd != nil {
			t.Fatal("有未结束任务时不该发删除")
		}
	}
	if !dialogContains(m, "不能删除") {
		t.Fatalf("缺少拒绝提示：%v", m.dialog)
	}
}

func TestSessionDeletedOpensFreshWhenCurrent(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeSessionDeleted, protocol.SessionDeletedPayload{SessionID: "ses_1"})
	if err != nil {
		t.Fatalf("构造 session.deleted: %v", err)
	}
	cmd := m.handleEnvelope(env)
	if cmd == nil {
		t.Fatal("当前会话被删后应新建会话")
	}
	if m.sessionID != "" {
		t.Fatalf("sessionID 应清空，得到 %q", m.sessionID)
	}
	if m.sessionsPanel != nil {
		t.Fatal("重置后不该留着旧面板")
	}
}

func TestSessionsPanelRowsShowWhen(t *testing.T) {
	now := time.Now()
	same := sessionWhen(now.UnixMilli())
	if len(same) != 5 || !strings.Contains(same, ":") {
		t.Fatalf("当天的会话应显示时刻，得到 %q", same)
	}
	old := sessionWhen(now.AddDate(0, 0, -40).UnixMilli())
	if len(old) != 5 || !strings.Contains(old, "-") {
		t.Fatalf("更早的会话应显示日期，得到 %q", old)
	}
	if sessionWhen(0) != "—" {
		t.Fatalf("零值应为占位符")
	}
}

func TestRenameCommandRenamesCurrentSession(t *testing.T) {
	m := newModel(t)
	m.sessionTitle = "旧标题"
	m.input = "/rename 新标题"

	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("应发 session.rename")
	}
	if m.input != "" || m.renamingSession != "" {
		t.Fatalf("提交后应清空输入与改名状态：input=%q renaming=%q", m.input, m.renamingSession)
	}
	// sendCmd 是延迟执行的，直接查它写出的帧。
	env := cmd()
	_ = env
	if !dialogContains(m, "") && m.input != "" {
		t.Fatal("输入行没清空")
	}
	if m.sessionTitle != "旧标题" {
		t.Fatalf("标题要等服务端回 session.renamed 才变，本地不该提前改：%q", m.sessionTitle)
	}
}

func TestRenameCommandWithoutArgumentPrefillsTitle(t *testing.T) {
	m := newModel(t)
	m.sessionTitle = "修登录超时"
	m.input = "/rename"

	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("不带参数时不该发请求")
	}
	if m.renamingSession != m.sessionID {
		t.Fatalf("应进入改名态：%q", m.renamingSession)
	}
	if m.input != "修登录超时" {
		t.Fatalf("输入行应填入现标题：%q", m.input)
	}
	if !dialogContains(m, "enter 保存") {
		t.Fatalf("缺少提示：%v", m.dialog)
	}

	// 接着输入并回车就是保存。
	m.input = "新名字"
	_, cmd = m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("回车后应提交改名")
	}
	if m.renamingSession != "" {
		t.Fatalf("提交后应退出改名态：%q", m.renamingSession)
	}
}

func TestRenameCommandRequiresSession(t *testing.T) {
	m := newModel(t)
	m.sessionID = ""
	m.input = "/rename 新标题"

	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("没有会话时不该发请求")
	}
	if !dialogContains(m, "尚未建立会话") {
		t.Fatalf("缺少提示：%v", m.dialog)
	}
}

func TestSessionRenamedEventUpdatesTitle(t *testing.T) {
	m := newModel(t)

	payload := json.RawMessage(`{"title":"改自命令行"}`)
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    string(domain.EventSessionRenamed),
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("构造事件帧: %v", err)
	}
	m.handleEnvelope(env)

	if m.sessionTitle != "改自命令行" {
		t.Fatalf("标题未跟随事件更新：%q", m.sessionTitle)
	}
	if bar := stripANSI(m.renderStatusBar(140, false)); !strings.Contains(bar, "▤ 改自命令行") {
		t.Fatalf("状态栏没更新：%s", bar)
	}
}

func TestSessionHeadlineTracksRename(t *testing.T) {
	m := newModel(t)

	// 新建会话：没有标题时不摆 id。
	m.sessionID = ""
	created, _ := protocol.New(protocol.TypeSessionCreated, protocol.SessionPayload{
		ID: "ses_headline", Workspace: "/tmp/ws",
	})
	m.handleEnvelope(created)
	if len(m.dialog) == 0 || !strings.Contains(m.dialog[0], "已进入会话（未命名") {
		t.Fatalf("入口表头不对：%v", m.dialog)
	}
	if strings.Contains(strings.Join(m.dialog, "\n"), "ses_headline") {
		t.Fatalf("表头不该显示会话 id：%v", m.dialog)
	}

	// 改名后表头就地更新，而不是再追加一行旧名字。
	renamed, _ := protocol.New(protocol.TypeSessionRenamed, protocol.SessionPayload{
		ID: "ses_headline", Title: "修登录超时",
	})
	m.handleEnvelope(renamed)
	if !strings.Contains(m.dialog[0], "已进入会话：修登录超时") {
		t.Fatalf("表头没跟着改名：%v", m.dialog)
	}

	// 事件路径（另一客户端改名 / 重放）同样改写表头。
	payload := json.RawMessage(`{"title":"改自命令行"}`)
	ev, _ := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind: string(domain.EventSessionRenamed), Payload: payload,
	})
	m.handleEnvelope(ev)
	if !strings.Contains(m.dialog[0], "已进入会话：改自命令行") {
		t.Fatalf("表头没跟随事件：%v", m.dialog)
	}
}

func TestExitCommandQuits(t *testing.T) {
	m := newModel(t)
	m.input = "/exit"

	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("/exit 应返回退出命令")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("/exit 应触发退出，实际 %T", cmd())
	}
	if m.input != "" {
		t.Fatalf("退出时输入行应清空：%q", m.input)
	}
}

func TestExitCommandWorksWithoutSession(t *testing.T) {
	m := newModel(t)
	m.sessionID = ""
	m.input = "/quit"

	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("没有会话时 /quit 也应能退出")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("/quit 应触发退出，实际 %T", cmd())
	}
}
