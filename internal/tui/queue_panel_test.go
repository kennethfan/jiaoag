package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// withTaskID 给事件信封补上 TaskID：eventEnvelope 只填 kind 与载荷，
// 而队列的出队判断依赖信封上的任务 id。
func withTaskID(t *testing.T, env protocol.Envelope, taskID string) protocol.Envelope {
	t.Helper()
	var p protocol.StreamEventPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码事件: %v", err)
	}
	p.TaskID = taskID
	rebuilt, err := protocol.New(protocol.TypeEvent, p)
	if err != nil {
		t.Fatalf("重建事件信封: %v", err)
	}
	rebuilt.Seq = env.Seq
	rebuilt.SessionID = env.SessionID
	return rebuilt
}

func queuedEvent(t *testing.T, seq int64, taskID, prompt string) protocol.Envelope {
	t.Helper()
	return withTaskID(t, eventEnvelope(t, seq, domain.EventTaskQueued,
		domain.TaskQueuedPayload{Prompt: prompt}), taskID)
}

func statusEvent(t *testing.T, seq int64, taskID string, from, to domain.TaskStatus) protocol.Envelope {
	t.Helper()
	return withTaskID(t, eventEnvelope(t, seq, domain.EventTaskStatus,
		domain.TaskStatusPayload{From: from, To: to}), taskID)
}

func queueIDsOf(m *Model) []domain.TaskID {
	out := make([]domain.TaskID, 0, len(m.queue))
	for _, q := range m.queue {
		out = append(out, q.ID)
	}
	return out
}

// 队列顺序必须是提交顺序（也就是执行顺序），并且界面能看到每一条。
func TestQueuePanelShowsFIFOOrder(t *testing.T) {
	m := newModel(t)
	prompts := []string{"修 README 的错别字", "补一个回归测试", "给执行器加日志"}
	for i, prompt := range prompts {
		m.handleEvent(queuedEvent(t, int64(i+1), fmt.Sprintf("tsk_q%d", i+1), prompt))
	}

	if len(m.queue) != 3 {
		t.Fatalf("队列长度 = %d，期望 3", len(m.queue))
	}
	want := []domain.TaskID{"tsk_q1", "tsk_q2", "tsk_q3"}
	for i, id := range want {
		if m.queue[i].ID != id {
			t.Fatalf("第 %d 项 = %s，期望 %s（顺序必须与提交一致）", i, m.queue[i].ID, id)
		}
	}
	if !strings.Contains(m.View().Content, "排队 3") {
		t.Fatalf("状态行未显示排队数量：\n%s", m.View().Content)
	}

	m.queuePanel = &queuePanelView{}
	panel := m.renderQueuePanel()
	for _, want := range []string{"[1]", "[2]", "[3]", prompts[0], prompts[1], prompts[2]} {
		if !strings.Contains(panel, want) {
			t.Fatalf("队列面板缺少 %q：\n%s", want, panel)
		}
	}
}

// 回执帧与服务端事件都会到达：同一任务只能登记一次。
func TestQueueDedupsReceiptAndEvent(t *testing.T) {
	m := newModel(t)
	receipt, err := protocol.New(protocol.TypeTaskQueued, protocol.TaskPayload{
		ID: "tsk_dup", Status: string(domain.TaskQueued), Prompt: "回执里的提问",
		CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cmd := m.handleEnvelope(receipt); cmd != nil {
		t.Fatalf("回执不应产生命令")
	}
	m.handleEvent(queuedEvent(t, 1, "tsk_dup", "回执里的提问"))

	if len(m.queue) != 1 {
		t.Fatalf("重复登记后队列长度 = %d，期望 1：%v", len(m.queue), queueIDsOf(m))
	}
	if m.queue[0].Prompt != "回执里的提问" {
		t.Fatalf("提问 = %q", m.queue[0].Prompt)
	}
}

// 取消只作用于选中的那一条，其它排队任务不动。
func TestQueueCancelSelectedOnly(t *testing.T) {
	m := newModel(t)
	for i := 1; i <= 3; i++ {
		m.handleEvent(queuedEvent(t, int64(i), fmt.Sprintf("tsk_q%d", i), fmt.Sprintf("任务 %d", i)))
	}
	m.queuePanel = &queuePanelView{selected: 1}

	_, cmd := m.handleQueueKey("x")
	if cmd == nil {
		t.Fatal("取消应发出 task.cancel 命令")
	}
	if !m.queue[1].cancelRequested {
		t.Fatal("选中项应标记已请求取消")
	}
	if m.queue[0].cancelRequested || m.queue[2].cancelRequested {
		t.Fatalf("其它排队任务被误标：%+v", m.queue)
	}
	if len(m.queue) != 3 {
		t.Fatalf("取消在终态事件之前不能移除条目，实际 %d 条", len(m.queue))
	}
}

// 移除等终态事件：取消请求被接受不等于任务已经结束。
func TestQueueRemovalWaitsForTerminalEvent(t *testing.T) {
	m := newModel(t)
	for i := 1; i <= 3; i++ {
		m.handleEvent(queuedEvent(t, int64(i), fmt.Sprintf("tsk_q%d", i), fmt.Sprintf("任务 %d", i)))
	}
	m.queuePanel = &queuePanelView{selected: 2}
	m.handleQueueKey("x")

	m.handleEvent(statusEvent(t, 10, "tsk_q3", domain.TaskQueued, domain.TaskCanceled))

	if m.queueIndex("tsk_q3") >= 0 {
		t.Fatalf("终态后应出队：%v", queueIDsOf(m))
	}
	if len(m.queue) != 2 {
		t.Fatalf("队列长度 = %d，期望 2", len(m.queue))
	}
	if m.queuePanel.selected != 1 {
		t.Fatalf("选中下标 = %d，应夹到 1", m.queuePanel.selected)
	}
}

// D 是两段式：先确认，再按 y；其它键视为放弃。
func TestQueueClearRequiresConfirm(t *testing.T) {
	m := newModel(t)
	for i := 1; i <= 3; i++ {
		m.handleEvent(queuedEvent(t, int64(i), fmt.Sprintf("tsk_q%d", i), fmt.Sprintf("任务 %d", i)))
	}
	m.queuePanel = &queuePanelView{}

	m.handleQueueKey("D")
	if !m.queuePanel.confirmClear {
		t.Fatal("D 应进入确认态")
	}
	m.handleQueueKey("n")
	if m.queuePanel.confirmClear {
		t.Fatal("确认态下按其它键应放弃")
	}
	for _, q := range m.queue {
		if q.cancelRequested {
			t.Fatalf("放弃后不应取消任何任务：%+v", m.queue)
		}
	}

	m.handleQueueKey("D")
	_, cmd := m.handleQueueKey("y")
	if cmd == nil {
		t.Fatal("确认后应批量发出取消命令")
	}
	for _, q := range m.queue {
		if !q.cancelRequested {
			t.Fatalf("确认后应标记全部排队任务：%+v", m.queue)
		}
	}
	if _, again := m.handleQueueKey("y"); again != nil {
		t.Fatal("已请求取消的任务不应重发 cancel")
	}
}

// 任务被拉起时自然出队，运行中的显示交给 runTaskID。
func TestQueueRemovesTaskWhenItStarts(t *testing.T) {
	m := newModel(t)
	m.handleEvent(queuedEvent(t, 1, "tsk_q1", "第一个"))
	m.handleEvent(queuedEvent(t, 2, "tsk_q2", "第二个"))

	m.handleEvent(statusEvent(t, 3, "tsk_q2", domain.TaskQueued, domain.TaskRunning))
	if m.queueIndex("tsk_q2") >= 0 {
		t.Fatalf("运行中的任务不应留在队列：%v", queueIDsOf(m))
	}
	if m.runTaskID != "tsk_q2" {
		t.Fatalf("runTaskID = %q，期望 tsk_q2", m.runTaskID)
	}
	if got := queueIDsOf(m); len(got) != 1 || got[0] != "tsk_q1" {
		t.Fatalf("剩余队列 = %v，期望 [tsk_q1]", got)
	}

	m.handleEvent(statusEvent(t, 4, "tsk_q2", domain.TaskRunning, domain.TaskCompleted))
	if m.runTaskID != "" {
		t.Fatalf("终态后 runTaskID 应清空，实际 %q", m.runTaskID)
	}
}

// 审批优先：approval.requested 到达时队列面板必须让位，否则用户会一边审 diff 一边管队列。
func TestQueuePanelYieldsToApproval(t *testing.T) {
	m := newModel(t)
	m.handleEvent(queuedEvent(t, 1, "tsk_q1", "排队中"))
	m.queuePanel = &queuePanelView{}

	m.handleEvent(eventEnvelope(t, 2, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_1",
		Kind:       domain.ApprovalDiff,
		Path:       "a.txt",
		Diff:       "+x\n",
	}))

	if m.queuePanel != nil {
		t.Fatal("审批到达后队列面板应关闭")
	}
	if m.approval == nil {
		t.Fatal("审批面板应打开")
	}
}

// esc 在队列面板里只关面板，不取消任务、不退出程序。
func TestQueuePanelEscOnlyCloses(t *testing.T) {
	m := newModel(t)
	m.handleEvent(queuedEvent(t, 1, "tsk_q1", "排队中"))
	m.queuePanel = &queuePanelView{}

	_, cmd := m.handleQueueKey("esc")
	if m.queuePanel != nil {
		t.Fatal("esc 应关闭队列面板")
	}
	if cmd != nil {
		t.Fatalf("esc 不应产生命令，实际 %T", cmd())
	}
	for _, q := range m.queue {
		if q.cancelRequested {
			t.Fatalf("esc 不应取消任务：%+v", m.queue)
		}
	}
}

// 断线重连后从事件流重建队列：顺序必须与服务端 FIFO 一致。
func TestQueueRebuildsFromReplay(t *testing.T) {
	m := newModel(t)
	m.resetView()

	m.handleEvent(queuedEvent(t, 1, "tsk_q1", "第一个"))
	m.handleEvent(queuedEvent(t, 2, "tsk_q2", "第二个"))
	m.handleEvent(queuedEvent(t, 3, "tsk_q3", "第三个"))
	m.handleEvent(statusEvent(t, 4, "tsk_q1", domain.TaskQueued, domain.TaskRunning))
	m.handleEvent(statusEvent(t, 5, "tsk_q1", domain.TaskRunning, domain.TaskCompleted))

	got := queueIDsOf(m)
	if len(got) != 2 || got[0] != "tsk_q2" || got[1] != "tsk_q3" {
		t.Fatalf("重建后的队列 = %v，期望 [tsk_q2 tsk_q3]", got)
	}
	if m.runTaskID != "" {
		t.Fatalf("已结束的任务不应占着 runTaskID：%q", m.runTaskID)
	}
}

// 空队列时 ctrl+q 只提示，不打开空面板；有任务时才打开。
func TestCtrlQOpensPanelOnlyWithQueue(t *testing.T) {
	m := newModel(t)

	m.handleKey(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	if m.queuePanel != nil {
		t.Fatal("空队列不应打开面板")
	}
	if !strings.Contains(m.dialog[len(m.dialog)-1], "队列为空") {
		t.Fatalf("空队列应提示，实际 %q", m.dialog)
	}

	m.handleEvent(queuedEvent(t, 1, "tsk_q1", "排队中"))
	m.handleKey(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	if m.queuePanel == nil {
		t.Fatal("有任务时 ctrl+q 应打开面板")
	}
}
