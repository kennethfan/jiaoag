package rpc_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
	"github.com/kennethfan/jiaoag/internal/store"
)

// TestSubscribeTailsEventsFromOtherRuntime 验证订阅能看到别的运行时写的事件：
// 两个 App 共用同一个库，a1 订阅后由 a2 改标题，事件只能从库里补拉。
func TestSubscribeTailsEventsFromOtherRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a1 := app.New(db, event.NewHub())
	a2 := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a1.Close() })
	t.Cleanup(func() { _ = a2.Close() })

	sess, err := a1.CreateSession(ctx, t.TempDir(), "", "", "尾巴")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	streams := rpc.NewStreams(a1, log)
	frames, stop, err := streams.Subscribe(ctx, protocol.EventSubscribePayload{SessionID: string(sess.ID), FromSeq: 0})
	if err != nil {
		t.Fatalf("订阅: %v", err)
	}
	t.Cleanup(stop)

	// 先消费掉回放的 session.created，避免把首帧当成补拉结果。
	if _, ok := <-frames; !ok {
		t.Fatal("订阅通道提前关闭")
	}

	if _, err := a2.RenameSession(ctx, sess.ID, "改自另一个运行时"); err != nil {
		t.Fatalf("改名: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("3 秒内没有从库里补拉到改名事件")
		case env, ok := <-frames:
			if !ok {
				t.Fatal("订阅通道提前关闭")
			}
			if env.Type != protocol.TypeEvent {
				continue
			}
			var p protocol.StreamEventPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				t.Fatalf("解析事件: %v", err)
			}
			if p.Kind != string(domain.EventSessionRenamed) {
				continue
			}
			var renamed domain.SessionRenamedPayload
			if err := json.Unmarshal(p.Payload, &renamed); err != nil {
				t.Fatalf("解析载荷: %v", err)
			}
			if renamed.Title != "改自另一个运行时" {
				t.Fatalf("标题 = %q", renamed.Title)
			}
			return
		}
	}
}
