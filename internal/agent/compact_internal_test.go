package agent

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
)

func compactedEvent(t *testing.T, summary string, covered int) domain.Event {
	t.Helper()
	ev, err := domain.NewEvent(domain.EventContextCompacted, "ses_1", "tsk_1",
		domain.ContextCompactedPayload{
			Summary:         summary,
			CoveredMessages: covered,
			BeforeChars:     5000,
		})
	if err != nil {
		t.Fatalf("构造压缩事件: %v", err)
	}
	return ev
}

// 重放时摘要替换被覆盖的前缀，但压缩之后、事件之前的近期消息必须留下。
func TestMessagesFromEventsAppliesCompaction(t *testing.T) {
	user1, _ := domain.NewEvent(domain.EventTaskQueued, "ses_1", "tsk_1", domain.TaskQueuedPayload{Prompt: "第一件事"})
	answer1, _ := domain.NewEvent(domain.EventAssistantMessage, "ses_1", "tsk_1",
		domain.AssistantMessagePayload{Text: "做完了"})
	answer2, _ := domain.NewEvent(domain.EventAssistantMessage, "ses_1", "tsk_1",
		domain.AssistantMessagePayload{Text: "正在做第二件事"})

	// 覆盖前两条（用户 + 助手），保留第三条。
	events := []domain.Event{user1, answer1, compactedEvent(t, "第一件事已完成", 2), answer2}
	msgs := MessagesFromEvents(events)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2：%+v", len(msgs), msgs)
	}
	if !isCompactedMessage(msgs[0]) {
		t.Fatalf("首条应是摘要：%+v", msgs[0])
	}
	if !strings.Contains(msgs[0].Blocks[0].Text, "第一件事已完成") {
		t.Fatalf("摘要内容丢失：%q", msgs[0].Blocks[0].Text)
	}
	if got := msgText(msgs[1]); got != "正在做第二件事" {
		t.Fatalf("近期消息被误删：%q", got)
	}
}

// Build 永远保留摘要，即使它自己就超出了预算。
func TestBuildPinsCompactedSummary(t *testing.T) {
	history := []llm.Message{
		compactedMessage("早前结论", 10),
		llm.TextMessage(llm.RoleUser, strings.Repeat("旧问题", 100)),
		llm.TextMessage(llm.RoleAssistant, strings.Repeat("旧回答", 100)),
		llm.TextMessage(llm.RoleUser, "当前问题"),
	}
	got := Build(history, 20)
	if len(got) == 0 || !isCompactedMessage(got[0]) {
		t.Fatalf("摘要被裁掉：%+v", got)
	}
	if last := got[len(got)-1]; msgText(last) != "当前问题" {
		t.Fatalf("最后一条用户消息被裁掉：%+v", got)
	}
}

func TestCompactSplitUsesTaskBoundary(t *testing.T) {
	msgs := []llm.Message{
		llm.TextMessage(llm.RoleUser, "任务一"),
		llm.TextMessage(llm.RoleAssistant, "答一"),
		llm.ToolResultMessage(llm.Block{Kind: llm.BlockToolResult, ToolCallID: "c1", Result: "r1"}),
		llm.TextMessage(llm.RoleUser, "任务二"),
		llm.TextMessage(llm.RoleAssistant, "答二"),
		llm.TextMessage(llm.RoleUser, "任务三"),
		llm.TextMessage(llm.RoleAssistant, "答三"),
	}
	if got, want := compactSplit(msgs, 2), 3; got != want {
		t.Fatalf("切分点 = %d，期望任务二起点 %d", got, want)
	}
}

// 单个任务内部没有新的用户消息时，退到助手消息边界，且不切开工具往返。
func TestCompactSplitFallsBackToAssistantBoundary(t *testing.T) {
	msgs := []llm.Message{
		llm.TextMessage(llm.RoleUser, "一个很长的任务"),
		llm.TextMessage(llm.RoleAssistant, "第一轮"),
		llm.ToolResultMessage(llm.Block{Kind: llm.BlockToolResult, ToolCallID: "c1", Result: "r1"}),
		llm.TextMessage(llm.RoleAssistant, "第二轮"),
		llm.ToolResultMessage(llm.Block{Kind: llm.BlockToolResult, ToolCallID: "c2", Result: "r2"}),
		llm.TextMessage(llm.RoleAssistant, "第三轮"),
	}
	if got, want := compactSplit(msgs, 2), 3; got != want {
		t.Fatalf("切分点 = %d，期望第二轮助手起点 %d", got, want)
	}
}

func TestCompactTranscriptClipsLongToolResult(t *testing.T) {
	long := strings.Repeat("x", compactToolResultChars*2)
	text := compactTranscript([]llm.Message{
		llm.ToolResultMessage(llm.Block{Kind: llm.BlockToolResult, ToolName: "read_file", Result: long}),
	})
	if len(text) >= len(long) {
		t.Fatalf("长工具结果没有截断：%d 字节", len(text))
	}
	if !strings.Contains(text, "已截断") {
		t.Fatalf("截断缺少标记：%q", text)
	}
}

func TestProviderCompactorSummaryText(t *testing.T) {
	c := providerCompactor{}
	if _, err := c.Compact(t.Context(), nil); err == nil {
		t.Fatal("没有 provider 时应报错")
	}
}

// msgText 只用于断言：把文本块拼起来。
func msgText(msg llm.Message) string {
	var b strings.Builder
	for _, block := range msg.Blocks {
		if block.Kind == llm.BlockText {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}
