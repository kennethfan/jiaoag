package domain

import "fmt"

// TaskStatus 是任务的生命周期状态。
type TaskStatus string

const (
	TaskQueued          TaskStatus = "queued"
	TaskRunning         TaskStatus = "running"
	TaskWaitingApproval TaskStatus = "waiting_approval"
	TaskCompleted       TaskStatus = "completed"
	TaskFailed          TaskStatus = "failed"
	TaskCanceled        TaskStatus = "canceled"
)

// taskTransitions 是唯一的状态迁移真源。
//
// 会话内同一时刻最多一个 running 任务由应用层保证；这里只保证单个任务的
// 状态不会走到不存在的分支。
var taskTransitions = map[TaskStatus][]TaskStatus{
	TaskQueued:          {TaskRunning, TaskCanceled},
	TaskRunning:         {TaskWaitingApproval, TaskCompleted, TaskFailed, TaskCanceled},
	TaskWaitingApproval: {TaskRunning, TaskFailed, TaskCanceled},
	TaskCompleted:       {},
	TaskFailed:          {},
	TaskCanceled:        {},
}

// Valid 报告状态是否是已知状态。
func (s TaskStatus) Valid() bool {
	_, ok := taskTransitions[s]
	return ok
}

// Terminal 报告状态是否已终止（不再接受迁移）。
func (s TaskStatus) Terminal() bool {
	next, ok := taskTransitions[s]
	return ok && len(next) == 0
}

// CanTransitionTo 报告从 s 到 next 是否是合法迁移。
func (s TaskStatus) CanTransitionTo(next TaskStatus) bool {
	for _, candidate := range taskTransitions[s] {
		if candidate == next {
			return true
		}
	}
	return false
}

// TransitionError 表示一次非法状态迁移。调用方应把它当作编程错误，
// 而不是用错误字符串去判断分支。
type TransitionError struct {
	From TaskStatus
	To   TaskStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("domain: illegal task transition %q → %q", e.From, e.To)
}

// CheckTransition 校验一次迁移，非法时返回 *TransitionError。
func CheckTransition(from, to TaskStatus) error {
	if !from.Valid() {
		return fmt.Errorf("domain: unknown task status %q", from)
	}
	if !to.Valid() {
		return fmt.Errorf("domain: unknown task status %q", to)
	}
	if !from.CanTransitionTo(to) {
		return &TransitionError{From: from, To: to}
	}
	return nil
}

// TransitionTo 就地迁移任务状态并维护时间戳；非法迁移不修改任务。
func (t *Task) TransitionTo(next TaskStatus) error {
	if err := CheckTransition(t.Status, next); err != nil {
		return err
	}
	now := Now()
	t.Status = next
	switch next {
	case TaskRunning:
		if t.StartedAt.IsZero() {
			t.StartedAt = now
		}
	case TaskCompleted, TaskFailed, TaskCanceled:
		t.FinishedAt = now
	}
	return nil
}
