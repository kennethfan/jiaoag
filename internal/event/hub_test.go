package event_test

import (
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
)

func makeEvent(t *testing.T, session domain.SessionID, seq int) domain.Event {
	t.Helper()
	ev, err := domain.NewEvent(domain.EventRoundStarted, session, "", domain.RoundStartedPayload{Round: seq})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	ev.Seq = domain.Seq(seq)
	return ev
}

func TestSubscribeReceivesPublished(t *testing.T) {
	hub := event.NewHub()
	defer hub.CloseAll()

	sub := hub.Subscribe("ses_1")
	defer sub.Close()

	hub.Publish([]domain.Event{makeEvent(t, "ses_1", 1), makeEvent(t, "ses_1", 2)})

	for want := 1; want <= 2; want++ {
		select {
		case ev := <-sub.C():
			if int(ev.Seq) != want {
				t.Fatalf("seq = %d，期望 %d", ev.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("超时未收到第 %d 条事件", want)
		}
	}
}

func TestPublishIgnoresOtherSessions(t *testing.T) {
	hub := event.NewHub()
	defer hub.CloseAll()

	sub := hub.Subscribe("ses_1")
	defer sub.Close()
	hub.Publish([]domain.Event{makeEvent(t, "ses_2", 1)})

	select {
	case ev := <-sub.C():
		t.Fatalf("收到其它会话的事件: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOverflowClosesSubscription(t *testing.T) {
	hub := event.NewHub()
	defer hub.CloseAll()

	sub := hub.Subscribe("ses_1")
	// 不消费，直到写满内部缓冲。
	batch := make([]domain.Event, 0, 300)
	for i := 0; i < 300; i++ {
		batch = append(batch, makeEvent(t, "ses_1", i+1))
	}
	hub.Publish(batch)

	select {
	case <-sub.Overflow():
	case <-time.After(time.Second):
		t.Fatal("缓冲写满应关闭 Overflow")
	}
	// 溢出后通道关闭，直到已缓冲的内容被读尽。
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-sub.C():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("溢出后通道应最终关闭")
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	hub := event.NewHub()
	sub := hub.Subscribe("ses_1")
	sub.Close()
	sub.Close()
	hub.Publish([]domain.Event{makeEvent(t, "ses_1", 1)})
}
