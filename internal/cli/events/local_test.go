package events

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

// 直连导出不需要服务端：造一个库、关掉写连接，只用只读句柄把事件读出来。
func TestRunLocalExportsWithoutServer(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jiaoag.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess := domain.Session{ID: "ses_local", Workspace: t.TempDir()}
	created, err := domain.NewEvent(domain.EventSessionCreated, sess.ID, "", domain.SessionCreatedPayload{Workspace: sess.Workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(ctx, sess, []domain.Event{created}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task := domain.Task{ID: "tsk_local", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "hello"}
	queued, err := domain.NewEvent(domain.EventTaskQueued, sess.ID, task.ID, domain.TaskQueuedPayload{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, task, []domain.Event{queued}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	var runErr error
	out := captureStdout(t, func() {
		runErr = runLocal(ctx, ro, path, options{
			sessionID: "ses_local",
			from:      0,
			auditFrom: 0,
			format:    "json",
			untilNow:  true,
		})
	})
	if runErr != nil {
		t.Fatalf("runLocal: %v", runErr)
	}
	for _, want := range []string{`"kind":"event"`, `"type":"session.created"`, `"type":"task.queued"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %s:\n%s", want, out)
		}
	}
}
