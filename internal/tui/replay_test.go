package tui

import (
	"encoding/json"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// 增量重连：保留历史与队列，只清掉不入库的流式残片和旧审批面板，
// 并从 lastSeq 之后继续订阅。
func TestIncrementalReconnectKeepsHistory(t *testing.T) {
	m := New(Options{Workspace: t.TempDir(), SessionID: "ses_keep"})
	m.lastSeq = 42
	m.taskID = "tsk_1"
	m.taskStatus = string(domain.TaskRunning)
	m.runTaskID = "tsk_1"
	m.dialog = []string{"旧历史"}
	m.stream = "断线前的半截输出"
	m.approval = &approvalView{id: "apr_old"}

	_, cmd := m.Update(connectedMsg{hello: protocol.HelloPayload{ServerVersion: "test"}})
	if cmd == nil {
		t.Fatal("增量重连应发起订阅")
	}
	if m.lastSeq != 42 {
		t.Fatalf("重连不应重置 lastSeq：%d", m.lastSeq)
	}
	if len(m.dialog) != 1 || m.dialog[0] != "旧历史" {
		t.Fatalf("增量重连清掉了历史：%v", m.dialog)
	}
	if m.runTaskID != "tsk_1" || m.taskStatus != string(domain.TaskRunning) {
		t.Fatalf("增量重连清掉了任务状态：run=%q status=%q", m.runTaskID, m.taskStatus)
	}
	if m.stream != "" {
		t.Fatalf("流式残片应清掉（delta 不入库，不能与重连后的输出拼接）：%q", m.stream)
	}
	if m.approval != nil {
		t.Fatal("旧审批面板应清掉，由快照重建真正待审批的项")
	}
}

// 首次连接（lastSeq=0）仍是全量重放：旧视图必须清空，避免与 replay 叠加。
func TestFirstReconnectStillResetsView(t *testing.T) {
	m := New(Options{Workspace: t.TempDir(), SessionID: "ses_keep"})
	m.dialog = []string{"旧历史"}
	m.lastSeq = 0

	_, cmd := m.Update(connectedMsg{hello: protocol.HelloPayload{ServerVersion: "test"}})
	if cmd == nil {
		t.Fatal("重连应发起订阅")
	}
	if len(m.dialog) != 0 || m.lastSeq != 0 {
		t.Fatalf("lastSeq=0 时应全量重放并清空视图：dialog=%v lastSeq=%d", m.dialog, m.lastSeq)
	}
}

// 增量回放补发的快照要能把断线前就已待审批的面板重新画出来。
func TestSnapshotRestoresPendingApprovalPanel(t *testing.T) {
	m := newModel(t)
	m.resetView()

	raw, err := json.Marshal(domain.ApprovalRequestedPayload{
		ApprovalID: "apr_pending",
		Kind:       domain.ApprovalDiff,
		Path:       "a.txt",
		Diff:       "@@ -1 +1 @@\n-a\n+b\n",
	})
	if err != nil {
		t.Fatalf("编码审批载荷: %v", err)
	}
	env, err := protocol.New(protocol.TypeEventSnapshot, protocol.EventSnapshotPayload{
		HeadSeq: 10,
		Pending: []protocol.StreamEventPayload{{
			Kind:    string(domain.EventApprovalRequested),
			Payload: raw,
		}},
	})
	if err != nil {
		t.Fatalf("构造快照: %v", err)
	}

	if cmd := m.handleEnvelope(env); cmd != nil {
		t.Fatal("非重置快照不应触发重新订阅")
	}
	if m.approval == nil || m.approval.id != "apr_pending" {
		t.Fatalf("快照没有恢复审批面板：%+v", m.approval)
	}
}

// 本地 seq 超过服务端 head：快照要求重置，客户端清空并从 0 重新订阅。
func TestSnapshotResetClearsViewAndResubscribes(t *testing.T) {
	m := New(Options{Workspace: t.TempDir(), SessionID: "ses_keep"})
	m.lastSeq = 99
	m.dialog = []string{"不属于这份日志的历史"}

	env, err := protocol.New(protocol.TypeEventSnapshot, protocol.EventSnapshotPayload{
		HeadSeq: 3,
		Reset:   true,
	})
	if err != nil {
		t.Fatalf("构造快照: %v", err)
	}
	cmd := m.handleEnvelope(env)
	if cmd == nil {
		t.Fatal("重置快照应重新发起 from_seq=0 订阅")
	}
	if m.lastSeq != 0 {
		t.Fatalf("重置后 lastSeq = %d，期望 0", m.lastSeq)
	}
	if len(m.dialog) != 0 {
		t.Fatalf("重置后应清空历史：%v", m.dialog)
	}
}
