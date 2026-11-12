package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func eventEnvelope(t *testing.T, seq int64, kind domain.EventKind, payload any) protocol.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化载荷: %v", err)
	}
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    string(kind),
		Payload: json.RawMessage(raw),
	})
	if err != nil {
		t.Fatalf("构造 event 信封: %v", err)
	}
	env.Seq = seq
	env.SessionID = "ses_1"
	return env
}

func newModel(t *testing.T) *Model {
	t.Helper()
	m := New(Options{Workspace: t.TempDir()})
	m.sessionID = "ses_1"
	m.taskID = "tsk_1"
	m.taskStatus = string(domain.TaskRunning)
	return m
}

func TestDeltaAccumulatesIntoStream(t *testing.T) {
	m := newModel(t)

	for _, text := range []string{"你好", "，世界"} {
		m.handleEvent(eventEnvelope(t, 0, domain.EventAssistantDelta, domain.AssistantDeltaPayload{Text: text}))
	}
	if m.stream != "你好，世界" {
		t.Fatalf("流式拼接结果 = %q", m.stream)
	}
	if len(m.dialog) != 0 {
		t.Fatalf("临时帧不应进入历史，实际 %v", m.dialog)
	}
}

func TestAssistantMessageReplacesStream(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 0, domain.EventAssistantDelta, domain.AssistantDeltaPayload{Text: "你说"}))
	m.handleEvent(eventEnvelope(t, 7, domain.EventAssistantMessage, domain.AssistantMessagePayload{
		Round:      1,
		Text:       "最终回答",
		StopReason: "end_turn",
		ToolCalls: []domain.AssistantToolCallPayload{
			{ID: "call_1", Name: "glob", Args: json.RawMessage(`{"pattern":"**/*.go"}`)},
		},
	}))

	if m.stream != "" {
		t.Fatalf("完整消息到达后应清空流式段，实际 %q", m.stream)
	}
	joined := strings.Join(m.dialog, "\n")
	for _, want := range []string{"jiaoag: 最终回答", "⚙ glob"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %q，实际：\n%s", want, joined)
		}
	}
}

func TestTaskStatusUpdatesStateAndTokens(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 9, domain.EventTaskStatus, domain.TaskStatusPayload{
		From:      domain.TaskRunning,
		To:        domain.TaskCompleted,
		PromptTok: 11,
		OutputTok: 5,
	}))

	if m.taskStatus != string(domain.TaskCompleted) {
		t.Fatalf("状态 = %s，期望 completed", m.taskStatus)
	}
	if m.tokens != 16 {
		t.Fatalf("token 合计 = %d，期望 16", m.tokens)
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "running → completed") {
		t.Fatalf("缺少状态迁移行：%v", m.dialog)
	}
}

func TestToolResultRendersStatusLine(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 8, domain.EventToolResult, domain.ToolResultPayload{
		Tool:       "read_file",
		ToolCallID: "call_1",
		Status:     "error",
		IsError:    true,
		DurationMS: 12,
	}))

	if !strings.Contains(strings.Join(m.dialog, "\n"), "↳ read_file 出错") {
		t.Fatalf("缺少工具行：%v", m.dialog)
	}
}

// 取消回执只表示请求被接受：运行中的任务可能仍在 running，本地不能提前置终态，
// 否则 cancelable() 立刻变 false，用户再提交会撞上 session_busy。
func TestCanceledAckDoesNotSetLocalStatus(t *testing.T) {
	m := newModel(t)
	ack, err := protocol.New(protocol.TypeTaskCanceled, protocol.TaskPayload{
		ID:        "tsk_1",
		SessionID: "ses_1",
		Status:    string(domain.TaskCanceled),
	})
	if err != nil {
		t.Fatalf("构造 task.canceled: %v", err)
	}
	m.handleEnvelope(ack)

	if m.taskStatus != string(domain.TaskRunning) {
		t.Fatalf("回执不应改本地状态，实际 %s", m.taskStatus)
	}
	if !m.cancelable() {
		t.Fatal("任务仍在运行时应保持可取消")
	}
}
