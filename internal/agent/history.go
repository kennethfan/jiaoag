package agent

import (
	"strings"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
)

// MessagesFromEvents 把事件日志还原成归一消息序列，是配置装配的输入。
//
// 只认四类事件：task.queued（用户提问）、assistant.message（助手输出）、
// tool.result（工具回填）、context.compacted（上下文摘要）。其余事件是过程
// 记录，不进上下文。流式增量从不出现在日志里，所以重连后得到的历史与当时一致。
//
// context.compacted 之前的消息会被整体丢弃并换成一条摘要消息：压缩是日志里
// 的事实，重放不需要再跑一次模型。
func MessagesFromEvents(events []domain.Event) []llm.Message {
	out := make([]llm.Message, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case domain.EventTaskQueued:
			var payload domain.TaskQueuedPayload
			if err := ev.Decode(&payload); err != nil || payload.Prompt == "" {
				continue
			}
			out = append(out, llm.TextMessage(llm.RoleUser, payload.Prompt))

		case domain.EventAssistantMessage:
			var payload domain.AssistantMessagePayload
			if err := ev.Decode(&payload); err != nil {
				continue
			}
			blocks := make([]llm.Block, 0, 1+len(payload.ToolCalls))
			if payload.Text != "" {
				blocks = append(blocks, llm.Block{Kind: llm.BlockText, Text: payload.Text})
			}
			for _, call := range payload.ToolCalls {
				blocks = append(blocks, llm.Block{
					Kind:       llm.BlockToolUse,
					ToolCallID: call.ID,
					ToolName:   call.Name,
					Args:       call.Args,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, llm.Message{Role: llm.RoleAssistant, Blocks: blocks})

		case domain.EventToolResult:
			var payload domain.ToolResultPayload
			if err := ev.Decode(&payload); err != nil {
				continue
			}
			out = append(out, llm.ToolResultMessage(llm.Block{
				Kind:       llm.BlockToolResult,
				ToolCallID: payload.ToolCallID,
				ToolName:   payload.Tool,
				Result:     payload.Output,
				IsError:    payload.IsError,
			}))

		case domain.EventContextCompacted:
			var payload domain.ContextCompactedPayload
			if err := ev.Decode(&payload); err != nil || strings.TrimSpace(payload.Summary) == "" {
				continue
			}
			// 摘要替换此前的 CoveredMessages 条消息；更近的轮次在日志里排在
			// 本事件之前，但不在覆盖范围内，必须保留。不能整段清空：那会让
			// 重放与压缩时的内存历史不一致。
			covered := payload.CoveredMessages
			if covered < 0 {
				covered = 0
			}
			if covered > len(out) {
				covered = len(out)
			}
			kept := append([]llm.Message(nil), out[covered:]...)
			out = append([]llm.Message{compactedMessage(payload.Summary, covered)}, kept...)
		}
	}
	return out
}
