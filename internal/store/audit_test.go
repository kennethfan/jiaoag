package store_test

import (
	"context"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// seedAudit 写两条审计：一条挂着工具调用（能关联到任务与会话），一条没有
// tool_call_id（例如 context.compact），用来验证 LEFT JOIN 不会把后者丢掉。
func seedAudit(t *testing.T, s interface {
	Apply(context.Context, domain.Transition) ([]domain.Event, error)
}, task domain.Task, sess domain.Session) {
	t.Helper()
	call := domain.ToolCall{ID: "tc_1", TaskID: task.ID, SessionID: sess.ID, Name: "run_command", Status: domain.ToolSucceeded}
	_, err := s.Apply(context.Background(), domain.Transition{
		TaskID:   task.ID,
		ToolCall: &call,
		Audit: []domain.AuditRecord{
			{Actor: "user", Action: "exec.approved", ToolCallID: call.ID, Detail: []byte(`{"argv":["go","test"]}`)},
			{Actor: "system", Action: "context.compact", Detail: []byte(`{"before_chars":4000}`)},
		},
		Events: []domain.Event{mustEvent(t, domain.EventTaskStatus, task, sess, domain.TaskStatusPayload{
			From: domain.TaskQueued, To: domain.TaskRunning,
		})},
	})
	if err != nil {
		t.Fatalf("写审计: %v", err)
	}
}

func mustEvent(t *testing.T, kind domain.EventKind, task domain.Task, sess domain.Session, payload any) domain.Event {
	t.Helper()
	ev, err := domain.NewEvent(kind, sess.ID, task.ID, payload)
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	return ev
}

func TestListAuditIncludesRecordsWithoutToolCall(t *testing.T) {
	s, _ := openStore(t)
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)
	seedAudit(t, s, task, sess)

	records, err := s.ListAudit(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("期望 2 条审计，得到 %d", len(records))
	}

	first := records[0]
	if first.Action != "exec.approved" {
		t.Fatalf("第一条应是 exec.approved，得到 %s", first.Action)
	}
	if first.TaskID != task.ID || first.SessionID != sess.ID {
		t.Fatalf("工具调用相关的审计应关联到任务与会话：task=%q session=%q", first.TaskID, first.SessionID)
	}
	if first.ToolCallID != "tc_1" {
		t.Fatalf("tool_call_id 丢了：%q", first.ToolCallID)
	}
	if len(first.Detail) == 0 {
		t.Fatal("detail 丢了")
	}

	second := records[1]
	if second.Action != "context.compact" {
		t.Fatalf("第二条应是 context.compact，得到 %s", second.Action)
	}
	if second.TaskID != "" {
		t.Fatalf("没有工具调用的审计不该凭空关联任务，得到 %q", second.TaskID)
	}
}

func TestListAuditUsesCursor(t *testing.T) {
	s, _ := openStore(t)
	sess := seedSession(t, s)
	task := seedTask(t, s, sess)
	seedAudit(t, s, task, sess)

	all, err := s.ListAudit(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	last := all[len(all)-1].ID

	rest, err := s.ListAudit(context.Background(), last, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("游标之后不该有记录，得到 %d", len(rest))
	}

	head, err := s.AuditHead(context.Background())
	if err != nil {
		t.Fatalf("AuditHead: %v", err)
	}
	if head != last {
		t.Fatalf("head=%d 应等于最后一条 id=%d", head, last)
	}
}

func TestAuditHeadOnEmptyTable(t *testing.T) {
	s, _ := openStore(t)
	head, err := s.AuditHead(context.Background())
	if err != nil {
		t.Fatalf("AuditHead: %v", err)
	}
	if head != 0 {
		t.Fatalf("空表 head 应为 0，得到 %d", head)
	}
}
