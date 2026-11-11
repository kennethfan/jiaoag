package agent

import (
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
)

// MessagesFromEvents 把事件日志还原成归一消息序列，是配置装配的输入。
//
// 只认三类事件：task.queued（用户提问）、assistant.message（助手输出）、
// tool.result（工具回填）。其余事件是过程记录，不进上下文。流式增量从不
// 出现在日志里，所以重连后得到的历史与当时一致。
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
		}
	}
	return out
}
