package tui

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// manageFrame 构造 session.manage.result 帧。顺序按服务端约定：updated_at 倒序
// （最近动过的在前）。
func manageFrame(t *testing.T, sessions ...protocol.SessionSummaryPayload) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(protocol.TypeSessionManageResult, protocol.SessionManageResultPayload{Sessions: sessions})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	return env
}

// 启动时把本目录的会话全部开成标签，并接到最近的那条上。
func TestStartupRestoresAllWorkspaceSessionsAsTabs(t *testing.T) {
	m := newModel(t)
	m.sessionID = ""
	m.restoring = true

	env := manageFrame(t,
		protocol.SessionSummaryPayload{ID: "ses_new", Workspace: m.opts.Workspace, Provider: "agnes",
			Model: "agnes-3.0-flash", Title: "修边界", Tasks: 3},
		protocol.SessionSummaryPayload{ID: "ses_old", Workspace: m.opts.Workspace, Title: "重构中间件", ActiveTasks: 1},
	)
	cmd := m.handleEnvelope(env)
	if cmd == nil {
		t.Fatal("应该发出 event.subscribe")
	}
	if m.restoring {
		t.Fatal("拿到清单后应清掉 restoring")
	}
	if m.sessionID != "ses_new" {
		t.Fatalf("应接到最近更新的会话，实际 %q", m.sessionID)
	}
	if m.sessionModel != "agnes-3.0-flash" || m.sessionProvider != "agnes" {
		t.Fatalf("模型信息没跟上：%s/%s", m.sessionProvider, m.sessionModel)
	}
	if len(m.tabs) != 2 {
		t.Fatalf("应把两条会话都开成标签：%+v", m.tabs)
	}
	// 标签按时间正序摆：新的在右。
	if m.tabs[0].ID != "ses_old" || m.tabs[1].ID != "ses_new" {
		t.Fatalf("标签顺序不对：%+v", m.tabs)
	}
	// 有未结束任务的那条在标签上标成运行中。
	if m.tabs[0].Status != string(domain.TaskRunning) {
		t.Fatalf("后台任务应在标签上显示运行中：%+v", m.tabs[0])
	}

	joined := strings.Join(m.dialog, "\n")
	// 表头只带标题，不摆会话 id。
	if !strings.Contains(joined, "继续会话：修边界") {
		t.Fatalf("缺少带标题的继续提示：%s", joined)
	}
	if strings.Contains(joined, "ses_new") {
		t.Fatalf("表头不该出现会话 id：%s", joined)
	}
	if !strings.Contains(joined, "已全部开成标签") {
		t.Fatalf("多会话时应提示标签操作：%s", joined)
	}
	strip := stripANSI(m.renderTabStrip(120))
	if !strings.Contains(strip, "修边界") || !strings.Contains(strip, "重构中间件") {
		t.Fatalf("标签行应含全部会话：%s", strip)
	}
}

// 单条会话时不开标签行，也不加「已全部开成标签」这类噪音提示。
func TestStartupSingleSessionHasNoTabStrip(t *testing.T) {
	m := newModel(t)
	m.sessionID = ""
	m.restoring = true

	m.handleEnvelope(manageFrame(t,
		protocol.SessionSummaryPayload{ID: "ses_only", Workspace: m.opts.Workspace, Title: "只有一条"},
	))
	if m.sessionID != "ses_only" || len(m.tabs) != 1 {
		t.Fatalf("应接到唯一一条：%q %+v", m.sessionID, m.tabs)
	}
	if strip := m.renderTabStrip(120); strip != "" {
		t.Fatalf("单条会话不该画标签行：%q", strip)
	}
	if strings.Contains(strings.Join(m.dialog, "\n"), "已全部开成标签") {
		t.Fatalf("单条会话不该提示标签操作：%v", m.dialog)
	}
}

func TestStartupWithoutSessionsCreatesNew(t *testing.T) {
	m := newModel(t)
	m.sessionID = ""
	m.restoring = true

	cmd := m.handleEnvelope(manageFrame(t))
	if cmd == nil {
		t.Fatal("没有会话时应发出 session.create")
	}
	if m.sessionID != "" {
		t.Fatalf("不应该凭空选一条，实际 %q", m.sessionID)
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "已新建") {
		t.Fatalf("应提示已新建：%v", m.dialog)
	}
}

// 首次连接先问清单，而不是直接新建。
func TestStartupAsksForSessionList(t *testing.T) {
	m := newModel(t)
	m.sessionID = "" // newModel 默认带一个会话 id，首次连接要模拟「没有」

	_, cmd := m.Update(connectedMsg{conn: nil})
	if cmd == nil {
		t.Fatal("应该发出请求帧")
	}
	if !m.restoring {
		t.Fatal("应进入等待清单状态")
	}
	if !strings.Contains(m.status, "正在载入") {
		t.Fatalf("状态行没提示：%q", m.status)
	}
}

// /sessions 面板自己发的 session.manage 回包不能被启动流程吃掉。
func TestSessionManageResultOpensPanelWhenNotRestoring(t *testing.T) {
	m := newModel(t)
	env := manageFrame(t, protocol.SessionSummaryPayload{ID: "ses_a", Workspace: m.opts.Workspace, Title: "甲"})
	if cmd := m.handleEnvelope(env); cmd != nil {
		t.Fatal("面板路径不该发命令")
	}
	if m.sessionsPanel == nil {
		t.Fatal("应打开会话面板")
	}
}
