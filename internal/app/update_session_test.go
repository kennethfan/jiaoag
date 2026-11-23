package app_test

import (
	"context"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

func TestUpdateSessionChangesModelAndRecordsEvent(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()

	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "测试")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	updated, err := a.UpdateSession(ctx, sess.ID, "agnes", "agnes-3.0-pro")
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	if updated.Provider != "agnes" || updated.Model != "agnes-3.0-pro" {
		t.Fatalf("会话未更新：%+v", updated)
	}
	if updated.UpdatedAt.Before(sess.UpdatedAt) {
		t.Fatalf("updated_at 未推进：%v < %v", updated.UpdatedAt, sess.UpdatedAt)
	}

	events, err := a.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != domain.EventSessionUpdated {
		t.Fatalf("最后一条事件 = %s，期望 %s", last.Type, domain.EventSessionUpdated)
	}
	var payload domain.SessionUpdatedPayload
	if err := last.Decode(&payload); err != nil {
		t.Fatalf("解码 session.updated: %v", err)
	}
	if payload.Provider != "agnes" || payload.Model != "agnes-3.0-pro" {
		t.Fatalf("事件载荷 = %+v", payload)
	}
}

func TestUpdateSessionRejectsEmptyModel(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := a.UpdateSession(ctx, sess.ID, "openai", "  "); err == nil {
		t.Fatal("空模型应被拒绝")
	}
}
