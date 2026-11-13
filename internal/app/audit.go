package app

import (
	"context"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// outputPath 把 tool_call_id 反查成全文日志路径，供 read_output 使用。
//
// 模型不能直接给路径：随机 tool_call_id 只在自己上下文里出现过，既不可枚举，
// 也不会跨会话泄漏。
func (a *App) outputPath(ctx context.Context, toolCallID string) (string, error) {
	call, err := a.store.GetToolCall(ctx, domain.ToolCallID(toolCallID))
	if err != nil {
		return "", err
	}
	if call.OutputPath == "" {
		return "", fmt.Errorf("工具调用 %s 没有输出文件", toolCallID)
	}
	return call.OutputPath, nil
}

// RecentAudit 返回某个任务最近 limit 条审计记录，供最小只读查询使用。
func (a *App) RecentAudit(ctx context.Context, taskID domain.TaskID, limit int) ([]domain.AuditRecord, error) {
	return a.store.RecentAudit(ctx, taskID, limit)
}
