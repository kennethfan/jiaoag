package app_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

func TestCreateSessionWithRootsRecordsEvent(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	primary := t.TempDir()
	lib := t.TempDir()

	sess, err := a.CreateSession(ctx, primary, "", "", "", domain.SessionRoot{Alias: "lib", Path: lib})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(sess.Roots) != 1 || sess.Roots[0].Alias != "lib" {
		t.Fatalf("会话未记录附加根：%+v", sess.Roots)
	}

	events, err := a.Replay(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var payload domain.SessionCreatedPayload
	for _, ev := range events {
		if ev.Type != domain.EventSessionCreated {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("解析 session.created: %v", err)
		}
	}
	if len(payload.Roots) != 1 || payload.Roots[0].Alias != "lib" {
		t.Fatalf("session.created 未带附加根：%+v", payload.Roots)
	}
}

func TestCreateSessionRejectsBadRoots(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	primary := t.TempDir()

	cases := []struct {
		name  string
		roots []domain.SessionRoot
	}{
		{"非法别名", []domain.SessionRoot{{Alias: "Lib", Path: t.TempDir()}}},
		{"重复别名", []domain.SessionRoot{{Alias: "lib", Path: t.TempDir()}, {Alias: "lib", Path: t.TempDir()}}},
		{"与主根相同", []domain.SessionRoot{{Alias: "lib", Path: primary}}},
		{"目录不存在", []domain.SessionRoot{{Alias: "lib", Path: primary + "/missing"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.CreateSession(ctx, primary, "", "", "", tc.roots...); err == nil {
				t.Fatal("应当报错")
			}
		})
	}
}
