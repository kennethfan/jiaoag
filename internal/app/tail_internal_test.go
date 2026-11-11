package app

import (
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

func tailEvent(kind domain.EventKind, seq int64) domain.Event {
	return domain.Event{Type: kind, SessionID: "ses_1", Seq: domain.Seq(seq)}
}

func TestTailEventsAlignsToTaskQueued(t *testing.T) {
	events := []domain.Event{
		tailEvent(domain.EventTaskQueued, 1),
		tailEvent(domain.EventAssistantMessage, 2),
		tailEvent(domain.EventToolResult, 3),
		tailEvent(domain.EventAssistantMessage, 4),
		tailEvent(domain.EventAssistantMessage, 5),
		tailEvent(domain.EventToolResult, 6),
		tailEvent(domain.EventTaskQueued, 7),
		tailEvent(domain.EventAssistantMessage, 8),
		tailEvent(domain.EventToolResult, 9),
		tailEvent(domain.EventTaskStatus, 10),
	}

	got := tailEvents(events, 4)
	if len(got) != 4 {
		t.Fatalf("保留条数 = %d，期望 4", len(got))
	}
	if got[0].Type != domain.EventTaskQueued {
		t.Fatalf("起点应对齐到 task.queued，实际 %s", got[0].Type)
	}
	if got[0].Seq != 7 || got[3].Seq != 10 {
		t.Fatalf("保留的不是最新一段：seq %d..%d", got[0].Seq, got[3].Seq)
	}
}

func TestTailEventsFallsBackWithoutTaskQueued(t *testing.T) {
	events := []domain.Event{
		tailEvent(domain.EventAssistantMessage, 1),
		tailEvent(domain.EventToolResult, 2),
		tailEvent(domain.EventAssistantMessage, 3),
		tailEvent(domain.EventToolResult, 4),
	}

	got := tailEvents(events, 2)
	if len(got) != 2 || got[0].Seq != 3 {
		t.Fatalf("窗口内没有 task.queued 时应退回硬切：%+v", got)
	}
}

func TestTailEventsKeepsShortHistory(t *testing.T) {
	events := []domain.Event{tailEvent(domain.EventTaskQueued, 1), tailEvent(domain.EventAssistantMessage, 2)}
	if got := tailEvents(events, 10); len(got) != len(events) {
		t.Fatalf("未超上限时不应裁剪，实际 %d 条", len(got))
	}
}
