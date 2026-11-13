package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// blockingProvider 一进入流式就发信号，然后挂到 ctx 取消为止。
//
// 用它证明「两个会话的任务确实同时在跑」，而不是先后结束。
type blockingProvider struct {
	entered chan struct{}
}

func (p *blockingProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试只走流式")
}

func (p *blockingProvider) ChatStream(ctx context.Context, _ llm.Request, _ func(llm.Chunk) error) error {
	p.entered <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func configureBlockingRuntime(t *testing.T, a *app.App, p llm.Provider) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return p, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:     3,
		ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

func waitTaskStatus(t *testing.T, a *app.App, id domain.TaskID, want domain.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := a.GetTask(context.Background(), id)
		if err != nil {
			t.Fatalf("读取任务 %s: %v", id, err)
		}
		if task.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("任务 %s 没有进入 %s", id, want)
}

// 跨会话并行：两个会话的任务同时处于 running；各自的事件 seq 单调且不串会话。
func TestCrossSessionTasksRunConcurrently(t *testing.T) {
	ts, application := newLedgerServer(t)
	_ = ts
	prov := &blockingProvider{entered: make(chan struct{}, 2)}
	configureBlockingRuntime(t, application, prov)

	ctx := context.Background()
	first, err := application.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	second, err := application.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	taskA, err := application.SubmitTask(ctx, first.ID, "A")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}
	taskB, err := application.SubmitTask(ctx, second.ID, "B")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}

	waitTaskStatus(t, application, taskA.ID, domain.TaskRunning)
	waitTaskStatus(t, application, taskB.ID, domain.TaskRunning)
	// 两个流式调用都进了 provider：证明两个任务在时间上重叠，而不是串行。
	for i := 0; i < 2; i++ {
		select {
		case <-prov.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("两个会话没有同时执行")
		}
	}

	for _, sess := range []domain.Session{first, second} {
		events, err := application.Events(ctx, sess.ID)
		if err != nil {
			t.Fatalf("读取事件: %v", err)
		}
		var last domain.Seq
		for _, ev := range events {
			if ev.SessionID != sess.ID {
				t.Fatalf("会话 %s 的日志混入了 %s 的事件", sess.ID, ev.SessionID)
			}
			if ev.Seq <= last {
				t.Fatalf("会话 %s 的 seq 不是单调递增：%d 在 %d 之后", sess.ID, ev.Seq, last)
			}
			last = ev.Seq
		}
	}

	// 收尾：取消两个会话，避免 Close 时等满 grace。
	_ = application.CancelTask(ctx, first.ID, taskA.ID)
	_ = application.CancelTask(ctx, second.ID, taskB.ID)
	waitTaskStatus(t, application, taskA.ID, domain.TaskCanceled)
	waitTaskStatus(t, application, taskB.ID, domain.TaskCanceled)
}

// collectReplay 读到 seq 连续的 count 条事件帧，返回起始 seq 与事件列表。
func collectReplay(t *testing.T, conn *websocket.Conn, count int) (int64, []protocol.StreamEventPayload) {
	t.Helper()
	var (
		first int64
		out   []protocol.StreamEventPayload
	)
	for i := 0; i < count; i++ {
		env := readEnvelope(t, conn)
		if env.Type != protocol.TypeEvent {
			t.Fatalf("回放第 %d 帧不是事件：%s", i+1, env.Type)
		}
		if i == 0 {
			first = env.Seq
		} else if env.Seq != first+int64(i) {
			t.Fatalf("回放缺号：第 %d 帧 seq = %d，期望 %d", i+1, env.Seq, first+int64(i))
		}
		var p protocol.StreamEventPayload
		if err := env.Decode(&p); err != nil {
			t.Fatalf("解码事件: %v", err)
		}
		out = append(out, p)
	}
	return first, out
}

// 重连一律 from_seq=0：断线前后的事件都要按原 seq 完整重放。
func TestReconnectReplaysFromZero(t *testing.T) {
	ts, application := newLedgerServer(t)
	ctx := context.Background()

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if _, err := application.SubmitTask(ctx, sess.ID, "断线前"); err != nil {
		t.Fatalf("提交任务: %v", err)
	}

	// 在线订阅只收到 live 帧，不看具体内容；这里模拟「断线时已收到一部分」。
	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)
	_ = awaitEvent(t, conn, domain.EventTaskQueued)

	conn.CloseNow()

	// 断线期间继续产生事件。
	if _, err := application.SubmitTask(ctx, sess.ID, "断线后"); err != nil {
		t.Fatalf("断线期间提交任务: %v", err)
	}

	conn2 := dial(t, wsURL(ts), testToken)
	handshake(t, conn2)
	sub2, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
		FromSeq:   0,
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn2, sub2)

	last, err := application.LastSeq(ctx, sess.ID)
	if err != nil {
		t.Fatalf("读取 last_seq: %v", err)
	}
	first, events := collectReplay(t, conn2, int(last))
	if first != 1 {
		t.Fatalf("全量回放应从头开始，实际从 %d 开始", first)
	}
	if events[0].Kind != string(domain.EventSessionCreated) {
		t.Fatalf("第一条事件 = %s，期望 session.created", events[0].Kind)
	}
	// 两条 task.queued 必须在回放里都出现：断线不该让历史缺段。
	queued := 0
	for _, ev := range events {
		if ev.Kind == string(domain.EventTaskQueued) {
			queued++
		}
	}
	if queued != 2 {
		t.Fatalf("回放里 task.queued 数量 = %d，期望 2", queued)
	}
}

// 审批等待跨断连：重连后从 seq=0 能看到 approval.requested，裁决仍能生效。
func TestApprovalSurvivesDisconnect(t *testing.T) {
	ts, application := newLedgerServer(t)
	configureWriteRuntime(t, application, &writeOnceProvider{})

	ctx := context.Background()
	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	task, err := application.SubmitTask(ctx, sess.ID, "写个文件")
	if err != nil {
		t.Fatalf("提交任务: %v", err)
	}

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
	requested := awaitEvent(t, conn, domain.EventApprovalRequested)
	req := decodeEvent[domain.ApprovalRequestedPayload](t, requested)

	// 停在待审批时断开：任务继续挂在 approvals future 上。
	conn.CloseNow()
	// 断连不取消任务：它仍挂在内存审批 future 上（ADR-0007）。
	if got, err := application.GetTask(ctx, task.ID); err != nil {
		t.Fatalf("GetTask: %v", err)
	} else if got.Status.Terminal() {
		t.Fatalf("断连把任务收尾成了 %s（%s）", got.Status, got.Error)
	}

	conn2 := dial(t, wsURL(ts), testToken)
	handshake(t, conn2)
	sub2, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
		FromSeq:   0,
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn2, sub2)

	// 全量重放里必须再次出现同一条 approval.requested。
	last, err := application.LastSeq(ctx, sess.ID)
	if err != nil {
		t.Fatalf("读取 last_seq: %v", err)
	}
	_, events := collectReplay(t, conn2, int(last))
	found := false
	for _, ev := range events {
		if ev.Kind != string(domain.EventApprovalRequested) {
			continue
		}
		p := decodeEvent[domain.ApprovalRequestedPayload](t, ev)
		if p.ApprovalID == req.ApprovalID {
			found = true
		}
	}
	if !found {
		t.Fatalf("重放里没有审批 %s", req.ApprovalID)
	}

	resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
		SessionID:  string(sess.ID),
		ApprovalID: string(req.ApprovalID),
		Decision:   string(domain.DecisionAllowOnce),
	})
	if err != nil {
		t.Fatalf("构造 approval.resolve: %v", err)
	}
	write(t, conn2, resolve)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		done, err := application.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if done.Status.Terminal() {
			if done.Status != domain.TaskCompleted {
				t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("断连重连后任务没有完成")
}
