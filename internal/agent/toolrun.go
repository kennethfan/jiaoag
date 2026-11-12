package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// recordPrepared 记录 tool.prepared，并把工具调用推进到下一状态。
//
// 副作用写进事件而不是只在内存里留着：回放时能回答「这次调用想改哪些文件」，
// 这比事后从参数里反推可靠。
func (a *Agent) recordPrepared(ctx context.Context, task domain.Task, toolCall *domain.ToolCall, prepared tools.Prepared) error {
	payload := domain.ToolPreparedPayload{
		Tool:  toolCall.Name,
		Paths: a.relPaths(prepared.Paths),
	}
	switch {
	case prepared.Change != nil:
		payload.SideEffect = string(tools.EffectProposeWrite)
		payload.Risk = domain.RiskMedium
		toolCall.Status = domain.ToolWaitingApprove
	default:
		payload.SideEffect = perm.Describe(prepared.Spec)
		toolCall.Status = domain.ToolRunning
	}
	ev, err := domain.NewEvent(domain.EventToolPrepared, task.SessionID, task.ID, payload)
	if err != nil {
		return err
	}
	return a.record(ctx, task, domain.Transition{
		ToolCall: toolCall,
		Events:   []domain.Event{ev},
	})
}

// finishTool 记录 tool.result 并返回给模型的结果块。
func (a *Agent) finishTool(ctx context.Context, task domain.Task, toolCall *domain.ToolCall, result tools.Result, started time.Time) (llm.Block, error) {
	elapsed := time.Since(started)

	status := "ok"
	if result.IsError {
		status = "error"
	}
	payload := domain.ToolResultPayload{
		Tool:       toolCall.Name,
		ToolCallID: string(toolCall.ID),
		Status:     status,
		IsError:    result.IsError,
		Truncated:  result.Truncated,
		Output:     result.Content,
		DurationMS: elapsed.Milliseconds(),
	}
	ev, err := domain.NewEvent(domain.EventToolResult, task.SessionID, task.ID, payload)
	if err != nil {
		return llm.Block{}, err
	}

	// 审批流程可能已经给出更准确的状态（denied / canceled），不要用 failed
	// 覆盖掉它——「用户拒绝」和「工具出错」在审计里是两件事。
	switch toolCall.Status {
	case domain.ToolDenied, domain.ToolCanceled:
	default:
		toolCall.Status = domain.ToolSucceeded
		if result.IsError {
			toolCall.Status = domain.ToolFailed
		}
	}
	toolCall.EndedAt = domain.Now()
	if result.IsError {
		toolCall.Error = firstLine(result.Content)
	}

	if err := a.record(ctx, task, domain.Transition{
		ToolCall: toolCall,
		Events:   []domain.Event{ev},
	}); err != nil {
		return llm.Block{}, err
	}
	return llm.Block{
		Kind:       llm.BlockToolResult,
		ToolCallID: string(toolCall.ID),
		ToolName:   toolCall.Name,
		Result:     result.Content,
		IsError:    result.IsError,
	}, nil
}

// relPaths 把工具给的绝对路径还原成工作区相对路径。
//
// 事件日志会长期保留、还会发给客户端，没有必要把用户的家目录结构写进去。
func (a *Agent) relPaths(abs []string) []string {
	if len(abs) == 0 {
		return nil
	}
	root := a.opts.Workspace.Root()
	out := make([]string, 0, len(abs))
	for _, p := range abs {
		rel, err := filepath.Rel(root, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// firstLine 取首行并截断，供 ToolCall.Error 这类短字段使用。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// describeApproved 是批准后回给模型的一句话：告诉它写到了哪、内容摘要变了。
func describeApproved(c *tools.Change) string {
	verb := "已修改"
	if !c.Exists {
		verb = "已新建"
	}
	return fmt.Sprintf("%s %s（内容摘要 %s）", verb, c.Path, patch.ShortHash(c.NewHash))
}
