package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

func TestTaskTransitions(t *testing.T) {
	tests := []struct {
		name string
		from domain.TaskStatus
		to   domain.TaskStatus
		ok   bool
	}{
		{"排队到运行", domain.TaskQueued, domain.TaskRunning, true},
		{"排队到取消", domain.TaskQueued, domain.TaskCanceled, true},
		{"排队不能直接完成", domain.TaskQueued, domain.TaskCompleted, false},
		{"排队不能等审批", domain.TaskQueued, domain.TaskWaitingApproval, false},
		{"运行到等审批", domain.TaskRunning, domain.TaskWaitingApproval, true},
		{"运行到完成", domain.TaskRunning, domain.TaskCompleted, true},
		{"运行到失败", domain.TaskRunning, domain.TaskFailed, true},
		{"运行到取消", domain.TaskRunning, domain.TaskCanceled, true},
		{"等审批回到运行", domain.TaskWaitingApproval, domain.TaskRunning, true},
		{"等审批到失败", domain.TaskWaitingApproval, domain.TaskFailed, true},
		{"等审批到取消", domain.TaskWaitingApproval, domain.TaskCanceled, true},
		{"等审批不能直接完成", domain.TaskWaitingApproval, domain.TaskCompleted, false},
		{"完成后不能再跑", domain.TaskCompleted, domain.TaskRunning, false},
		{"失败后不能重来", domain.TaskFailed, domain.TaskRunning, false},
		{"取消后不能重来", domain.TaskCanceled, domain.TaskRunning, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CheckTransition(tc.from, tc.to)
			if tc.ok && err != nil {
				t.Fatalf("%s → %s 期望合法，得到 %v", tc.from, tc.to, err)
			}
			if !tc.ok {
				var te *domain.TransitionError
				if !errors.As(err, &te) {
					t.Fatalf("%s → %s 期望 *TransitionError，得到 %v", tc.from, tc.to, err)
				}
			}
		})
	}
}

func TestUnknownStatusIsRejected(t *testing.T) {
	if domain.TaskStatus("bogus").Valid() {
		t.Fatal("未知状态不应合法")
	}
	if err := domain.CheckTransition(domain.TaskQueued, "bogus"); err == nil {
		t.Fatal("未知目标状态应报错")
	}
	if !domain.TaskCompleted.Terminal() || domain.TaskRunning.Terminal() {
		t.Fatal("终态判断错误")
	}
}

func TestTransitionToMaintainsTimestamps(t *testing.T) {
	task := domain.Task{Status: domain.TaskQueued}
	if err := task.TransitionTo(domain.TaskRunning); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if task.StartedAt.IsZero() {
		t.Fatal("进入 running 应填 started_at")
	}
	started := task.StartedAt
	if err := task.TransitionTo(domain.TaskCompleted); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !task.StartedAt.Equal(started) {
		t.Fatal("started_at 不应被覆盖")
	}
	if task.FinishedAt.IsZero() {
		t.Fatal("进入终态应填 finished_at")
	}
}

func TestEventRoundTrip(t *testing.T) {
	ev, err := domain.NewEvent(domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{
		From: domain.TaskRunning,
		To:   domain.TaskCompleted,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if ev.V != domain.EventVersion {
		t.Fatalf("版本 = %d", ev.V)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("校验: %v", err)
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("编码事件: %v", err)
	}
	var back domain.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("解码事件: %v", err)
	}
	var p domain.TaskStatusPayload
	if err := back.Decode(&p); err != nil {
		t.Fatalf("解码载荷: %v", err)
	}
	if p.From != domain.TaskRunning || p.To != domain.TaskCompleted {
		t.Fatalf("载荷往返丢失: %+v", p)
	}
}

func TestEventValidation(t *testing.T) {
	if err := (domain.Event{Type: domain.EventError, SessionID: "ses"}).Validate(); err != nil {
		t.Fatalf("合法事件被拒: %v", err)
	}
	if err := (domain.Event{SessionID: "ses"}).Validate(); err == nil {
		t.Fatal("缺少类型应报错")
	}
	if err := (domain.Event{Type: domain.EventError}).Validate(); err == nil {
		t.Fatal("缺少会话应报错")
	}
	if err := (domain.Event{Type: domain.EventError, SessionID: "ses", Payload: json.RawMessage("{")}).Validate(); err == nil {
		t.Fatal("非法 JSON 载荷应报错")
	}
}
