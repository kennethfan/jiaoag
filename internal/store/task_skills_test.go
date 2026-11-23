package store_test

import (
	"context"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// 技能名随任务落库：重启续跑要能重新加载同一份正文。
func TestCreateTaskPersistsSkills(t *testing.T) {
	s, _ := openStore(t)
	sess := seedSession(t, s)

	task := domain.Task{
		ID:        "tsk_skills",
		SessionID: sess.ID,
		Status:    domain.TaskQueued,
		Prompt:    "帮我 grill 这个方案",
		Skills:    []string{"grilling", "codebase-design"},
	}
	ev, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, task.ID, domain.TaskQueuedPayload{
		Prompt: task.Prompt,
		Skills: task.Skills,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	if _, err := s.CreateTask(context.Background(), task, []domain.Event{ev}); err != nil {
		t.Fatalf("创建任务: %v", err)
	}

	got, err := s.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("读取任务: %v", err)
	}
	if len(got.Skills) != 2 || got.Skills[0] != "grilling" || got.Skills[1] != "codebase-design" {
		t.Fatalf("技能列表 = %v", got.Skills)
	}

	// 没有技能的任务读回来应是 nil，而不是空串占位。
	plain := domain.Task{ID: "tsk_plain", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "普通任务"}
	if _, err := s.CreateTask(context.Background(), plain, nil); err != nil {
		t.Fatalf("创建普通任务: %v", err)
	}
	got, err = s.GetTask(context.Background(), plain.ID)
	if err != nil {
		t.Fatalf("读取普通任务: %v", err)
	}
	if len(got.Skills) != 0 {
		t.Fatalf("普通任务不该有技能：%v", got.Skills)
	}
}
