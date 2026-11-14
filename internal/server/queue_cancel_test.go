package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// gatedProvider 每次流式都要等一次放行：把「哪个任务在跑」变成测试可控的状态。
type gatedProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *gatedProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试只走流式")
}

func (p *gatedProvider) ChatStream(ctx context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	p.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	if err := fn(llm.Chunk{TextDelta: "好的"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd})
}

// 取消排队中的任务不能打乱 FIFO：第 2 个被取消后，第 1 个照常完成、第 3 个接上。
// 这是 TUI 队列面板的服务端契约（面板只发 task.cancel，顺序由 app 保证）。
func TestCancelQueuedTaskKeepsFIFOOrder(t *testing.T) {
	ts, application := newLedgerServer(t)
	prov := &gatedProvider{entered: make(chan struct{}, 3), release: make(chan struct{})}
	configureBlockingRuntime(t, application, prov)

	ctx := context.Background()
	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "test-model", "队列取消")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	first, err := application.SubmitTask(ctx, sess.ID, "第一个")
	if err != nil {
		t.Fatalf("提交第一个: %v", err)
	}
	waitTaskStatus(t, application, first.ID, domain.TaskRunning)
	<-prov.entered

	second, err := application.SubmitTask(ctx, sess.ID, "第二个")
	if err != nil {
		t.Fatalf("提交第二个: %v", err)
	}
	third, err := application.SubmitTask(ctx, sess.ID, "第三个")
	if err != nil {
		t.Fatalf("提交第三个: %v", err)
	}
	if second.Status != domain.TaskQueued || third.Status != domain.TaskQueued {
		t.Fatalf("后两个任务应立即排队，实际 %s / %s", second.Status, third.Status)
	}

	// 走协议取消排队中的第二个：TUI 面板做的正是这件事。
	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)
	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
		FromSeq:   0,
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)
	cancel, err := protocol.New(protocol.TypeTaskCancel, protocol.TaskCancelPayload{
		SessionID: string(sess.ID),
		TaskID:    string(second.ID),
	})
	if err != nil {
		t.Fatalf("构造 task.cancel: %v", err)
	}
	write(t, conn, cancel)

	// 终态以 task.status 事件为准（回执只表示请求被接受）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("等不到第二个任务的 canceled 事件")
		}
		env, ok := readEnvelopeWithin(conn, remaining)
		if !ok {
			t.Fatal("等待取消事件超时")
		}
		if env.Type != protocol.TypeEvent {
			continue
		}
		var p protocol.StreamEventPayload
		if err := env.Decode(&p); err != nil {
			t.Fatalf("解码事件: %v", err)
		}
		if p.Kind != string(domain.EventTaskStatus) || p.TaskID != string(second.ID) {
			continue
		}
		var s domain.TaskStatusPayload
		if err := json.Unmarshal(p.Payload, &s); err != nil {
			t.Fatalf("解码 task.status: %v", err)
		}
		if s.To == domain.TaskCanceled {
			break
		}
	}
	waitTaskStatus(t, application, second.ID, domain.TaskCanceled)

	// 放行第一个：它必须照常完成，然后第三个接上（第二个永远不该被拉起）。
	prov.release <- struct{}{}
	waitTaskStatus(t, application, first.ID, domain.TaskCompleted)
	waitTaskStatus(t, application, third.ID, domain.TaskRunning)

	prov.release <- struct{}{}
	waitTaskStatus(t, application, third.ID, domain.TaskCompleted)

	if len(prov.entered) != 1 {
		// 第一个与第三个各进入一次流式；这里剩一条说明第三个也已开始。
		t.Logf("仍有一次流式未进入（第三个刚开始）：%d", len(prov.entered))
	}
	final, err := application.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatalf("读取第二个任务: %v", err)
	}
	if final.Status != domain.TaskCanceled {
		t.Fatalf("第二个任务状态 = %s，期望 canceled", final.Status)
	}
}
