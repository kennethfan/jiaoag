package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// 提交第二个任务时收到的 task.queued 回执不能覆盖正在跑的任务。
func TestQueuedReplyKeepsRunningTask(t *testing.T) {
	m := newModel(t)
	m.runTaskID = "tsk_run"
	m.taskID = "tsk_run"
	m.taskStatus = string(domain.TaskRunning)
	m.stream = "半截输出"

	env, err := protocol.New(protocol.TypeTaskQueued, protocol.TaskPayload{
		ID:        "tsk_q",
		SessionID: "ses_1",
		Status:    string(domain.TaskQueued),
	})
	if err != nil {
		t.Fatalf("构造回执: %v", err)
	}
	m.handleEnvelope(env)

	if m.runTaskID != "tsk_run" {
		t.Fatalf("运行中任务被排队回执覆盖成 %q", m.runTaskID)
	}
	if m.taskID != "tsk_run" || m.taskStatus != string(domain.TaskRunning) {
		t.Fatalf("头部显示被排队回执抢占：task=%q status=%q", m.taskID, m.taskStatus)
	}
	if m.stream != "半截输出" {
		t.Fatalf("排队回执清掉了流式文本：%q", m.stream)
	}
	if _, ok := m.queuedIDs[domain.TaskID("tsk_q")]; !ok {
		t.Fatalf("排队任务没有登记：%v", m.queuedIDs)
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "排队") {
		t.Fatalf("排队回执应给出提示：%v", m.dialog)
	}
}

// 排队任务的终态只影响它自己：运行中任务的显示与流式文本不能被清掉。
func TestQueuedTaskTerminalKeepsRunningStream(t *testing.T) {
	m := newModel(t)
	m.runTaskID = "tsk_run"
	m.taskID = "tsk_run"
	m.taskStatus = string(domain.TaskRunning)
	m.queuedIDs = map[domain.TaskID]struct{}{"tsk_q": {}}
	m.stream = "半截输出"

	raw, err := json.Marshal(domain.TaskStatusPayload{From: domain.TaskQueued, To: domain.TaskCanceled})
	if err != nil {
		t.Fatalf("序列化状态: %v", err)
	}
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		TaskID:  "tsk_q",
		Kind:    string(domain.EventTaskStatus),
		Payload: raw,
	})
	if err != nil {
		t.Fatalf("构造状态事件: %v", err)
	}
	env.Seq = 11
	m.handleEvent(env)

	if m.stream != "半截输出" {
		t.Fatalf("排队任务的终态清掉了运行中任务的流式文本：%q", m.stream)
	}
	if m.taskStatus != string(domain.TaskRunning) || m.runTaskID != "tsk_run" {
		t.Fatalf("排队任务终态改动了运行中任务：task=%q run=%q status=%q",
			m.taskID, m.runTaskID, m.taskStatus)
	}
	if _, ok := m.queuedIDs["tsk_q"]; ok {
		t.Fatalf("终态后排队任务未出队：%v", m.queuedIDs)
	}
}

// esc 只取消正在执行的任务；排队任务不能成为取消目标。
func TestEscTargetsRunningTaskOnly(t *testing.T) {
	m := newModel(t)
	m.queuedIDs = map[domain.TaskID]struct{}{"tsk_q": {}}

	// 只有排队任务时 esc 仍是「退出」，不会误取消排队任务。
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if msg := cmd(); msg == nil {
		t.Fatal("esc 应产生命令")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("没有运行中任务时 esc 应退出，实际 %T", msg)
	}

	m.runTaskID = "tsk_run"
	_, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if msg := cmd(); msg == nil {
		t.Fatal("esc 应产生命令")
	} else if _, ok := msg.(tea.QuitMsg); ok {
		t.Fatal("有运行中任务时 esc 应发取消，而不是退出")
	}
	if m.runTaskID != "tsk_run" {
		t.Fatalf("esc 不应改动运行中任务 id，实际 %q", m.runTaskID)
	}
}

// 运行中仍可以提交：服务端排队，客户端不该在本地拦截。
// 提问的回显由 task.queued 事件负责，这样重连全量重放也能重建用户原话。
func TestEnterSubmitsWhileRunning(t *testing.T) {
	m := newModel(t)
	m.input = "第二个任务"

	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("运行中提交应发 task.submit")
	}
	if m.input != "" {
		t.Fatalf("提交后输入行未清空：%q", m.input)
	}
	if len(m.dialog) != 0 {
		t.Fatalf("提交不应本地回显，提问交给 task.queued 事件：%v", m.dialog)
	}

	m.handleEvent(eventEnvelope(t, 9, domain.EventTaskQueued, domain.TaskQueuedPayload{Prompt: "第二个任务"}))
	if len(m.dialog) != 1 || m.dialog[0] != "› 第二个任务" {
		t.Fatalf("task.queued 事件应渲染用户提问：%v", m.dialog)
	}
}

// 全量重放要能重建用户提问（客户端不在本地保存对话历史）。
func TestReplayRendersUserPrompt(t *testing.T) {
	m := newModel(t)
	m.resetView()

	m.handleEvent(eventEnvelope(t, 3, domain.EventTaskQueued, domain.TaskQueuedPayload{Prompt: "帮我看看 go.mod"}))
	if len(m.dialog) != 1 || m.dialog[0] != "› 帮我看看 go.mod" {
		t.Fatalf("重放缺用户提问：%v", m.dialog)
	}
}

// 断线重连复用会话并从 seq=0 全量重放，不能清掉 sessionID 重新建会话。
func TestReconnectReusesSessionAndResetsView(t *testing.T) {
	m := New(Options{Workspace: t.TempDir(), SessionID: "ses_keep"})
	m.taskID = "tsk_1"
	m.taskStatus = string(domain.TaskRunning)
	m.runTaskID = "tsk_1"
	m.dialog = []string{"旧历史"}
	m.stream = "旧流式"

	_, cmd := m.Update(connectedMsg{hello: protocol.HelloPayload{ServerVersion: "test"}})
	if cmd == nil {
		t.Fatal("重连应发起订阅")
	}
	if m.sessionID != "ses_keep" {
		t.Fatalf("重连丢了会话：%q", m.sessionID)
	}
	if m.runTaskID != "" || m.taskStatus != "" || m.stream != "" || len(m.dialog) != 0 {
		t.Fatalf("重连应清空本地视图再回放：run=%q status=%q stream=%q dialog=%v",
			m.runTaskID, m.taskStatus, m.stream, m.dialog)
	}
}

// 没有 --session 时仍走 session.create（sessionID 在收到回执前保持为空）。
func TestFreshConnectCreatesSession(t *testing.T) {
	m := New(Options{Workspace: t.TempDir()})
	if m.sessionID != "" {
		t.Fatalf("新客户端不应带会话 id，实际 %q", m.sessionID)
	}
	if _, cmd := m.Update(connectedMsg{hello: protocol.HelloPayload{ServerVersion: "test"}}); cmd == nil {
		t.Fatal("首次连接应发 session.create")
	}
	if m.sessionID != "" {
		t.Fatalf("建会话前不应凭空得到 id：%q", m.sessionID)
	}
}

// 全量重放要能把「待审批」重新画出来。
func TestReplayRestoresApprovalPanel(t *testing.T) {
	m := newModel(t)
	m.resetView()

	m.handleEvent(eventEnvelope(t, 12, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_1",
		Kind:       domain.ApprovalDiff,
		Path:       "a.txt",
		Diff:       "@@ -1 +1 @@\n-a\n+b\n",
	}))
	if m.approval == nil {
		t.Fatal("重放应重建审批面板")
	}
	if m.approval.id != "apr_1" {
		t.Fatalf("重建的面板 id = %q", m.approval.id)
	}
}
