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
	case prepared.Command != nil:
		payload.SideEffect = string(tools.EffectProcess)
		payload.Risk = domain.RiskHigh
		payload.Command = prepared.Command.Argv
		// 到这里只说明命令准备完毕，还没决定要不要审批，进程一个字节都没起。
		// 写成 running 会让「待审批」和「正在执行」在投影里分不开：崩溃或取消
		// 之后这条记录会永远停在 running，看上去像是跑过。真正的
		// waiting_approval / running 由判定之后的分支产生。
		toolCall.Status = domain.ToolPrepared
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
//
// audits 与 tool.result 同事务落库：审计不能出现「有执行、没记录」的空洞。
func (a *Agent) finishTool(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	result tools.Result, started time.Time, audits []domain.AuditRecord) (llm.Block, error) {

	elapsed := time.Since(started)

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

	// 事件里的 status 直接取工具调用的终态，而不是从 IsError 反推：
	// denied / canceled / failed 是三种结局，压成同一个 "error" 之后，
	// 回放与界面就再也回答不了「这次调用为什么没跑」。
	payload := domain.ToolResultPayload{
		Tool:       toolCall.Name,
		ToolCallID: string(toolCall.ID),
		Status:     string(toolCall.Status),
		IsError:    result.IsError,
		Truncated:  result.Truncated,
		Output:     result.Content,
		DurationMS: elapsed.Milliseconds(),
		ExitCode:   result.ExitCode,
		OutputPath: result.OutputPath,
	}
	ev, err := domain.NewEvent(domain.EventToolResult, task.SessionID, task.ID, payload)
	if err != nil {
		return llm.Block{}, err
	}
	toolCall.EndedAt = domain.Now()
	if result.IsError {
		toolCall.Error = firstLine(result.Content)
	}
	toolCall.OutputPath = result.OutputPath
	toolCall.ExitCode = result.ExitCode

	// 终态不跟随父 ctx：用户刚按下取消时，这条工具结局正是最需要落库的。
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		ToolCall: toolCall,
		Audit:    audits,
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

// abortTool 给「准备之后、结果之前就断了」的调用补一个终态。
//
// 绝大多数工具错误已经被 Registry.Execute 降级成结果，这里兜的是取消：
// ctx 一旦结束，Execute 会把 ctx.Err() 抛上来。此时任务会终止，但工具调用
// 不能停在 running / waiting_approval，历史里也不能留下没有回填的 tool_use。
func (a *Agent) abortTool(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	prepared tools.Prepared, decision perm.Decision, started time.Time, cause error) (llm.Block, error) {

	switch toolCall.Status {
	case domain.ToolSucceeded, domain.ToolFailed, domain.ToolDenied, domain.ToolCanceled:
		// 已经收尾（例如审批取消路径自己写过结果）：不要再补第二个结果。
		return llm.Block{}, cause
	}

	status := domain.ToolFailed
	text := fmt.Sprintf("%s 执行失败：%v", toolCall.Name, cause)
	if ctx.Err() != nil {
		status = domain.ToolCanceled
		text = fmt.Sprintf("%s 已取消，未完成：%v", toolCall.Name, cause)
	}
	toolCall.Status = status

	var audits []domain.AuditRecord
	if prepared.Command != nil {
		// 命令没有跑到「有退出码」这一步：审计里记执行失败，而不是策略放行。
		audits = append(audits, execAudit(auditExecFailed, "system", prepared,
			tools.Result{IsError: true}, decision))
	}
	if _, err := a.finishTool(ctx, task, toolCall, tools.Result{Content: text, IsError: true}, started, audits); err != nil {
		return llm.Block{}, err
	}
	return llm.Block{}, cause
}

// toolResultEvent 为「等不到裁决就结束」的工具调用补一条 tool.result。
//
// 每个 tool_use 都必须有且只有一条 tool_result：没有结果的那次调用会让回放
// 出的历史变成「assistant 要求调用、却没有任何结果」，下一次请求会被供应商
// 直接拒绝；日志也回答不了「这次调用怎么了」。
func toolResultEvent(task domain.Task, toolCall *domain.ToolCall,
	status domain.ToolCallStatus, output string) (domain.Event, error) {

	return domain.NewEvent(domain.EventToolResult, task.SessionID, task.ID, domain.ToolResultPayload{
		Tool:       toolCall.Name,
		ToolCallID: string(toolCall.ID),
		Status:     string(status),
		IsError:    true,
		Output:     output,
	})
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
