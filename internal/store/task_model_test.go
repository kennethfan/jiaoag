package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

func TestCreateTaskPersistsModelOverride(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer s.Close()

	sess, err := s.CreateSession(ctx, domain.Session{
		ID:        "ses_1",
		Workspace: t.TempDir(),
		Provider:  "openai",
		Model:     "gpt-x",
	}, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(sess) != 0 {
		t.Fatalf("CreateSession 返回了意外事件：%d 条", len(sess))
	}

	_, err = s.CreateTask(ctx, domain.Task{
		ID:        "tsk_1",
		SessionID: "ses_1",
		Status:    domain.TaskQueued,
		Prompt:    "单次模型",
		Provider:  "agnes",
		Model:     "agnes-3.0-flash",
	}, nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task, err := s.GetTask(ctx, "tsk_1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Provider != "agnes" || task.Model != "agnes-3.0-flash" {
		t.Fatalf("模型覆盖未持久化：%s/%s", task.Provider, task.Model)
	}

	// 没覆盖的任务读回来是空串，表示「跟随会话」。
	if _, err := s.CreateTask(ctx, domain.Task{
		ID: "tsk_2", SessionID: "ses_1", Status: domain.TaskQueued, Prompt: "默认",
	}, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	plain, err := s.GetTask(ctx, "tsk_2")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if plain.Provider != "" || plain.Model != "" {
		t.Fatalf("未覆盖的任务读回非空：%s/%s", plain.Provider, plain.Model)
	}
}
