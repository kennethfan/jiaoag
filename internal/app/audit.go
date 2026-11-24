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

// ListAudit 按 id 升序返回 afterID 之后的审计记录。
//
// 只读：审计是事后追责的依据，导出通道不能顺带改任何东西。
func (a *App) ListAudit(ctx context.Context, afterID int64, limit int) ([]domain.AuditRecord, error) {
	return a.store.ListAudit(ctx, afterID, limit)
}

// AuditHead 返回当前最大的审计 id，供消费者从「现在」开始 tail。
func (a *App) AuditHead(ctx context.Context) (int64, error) {
	return a.store.AuditHead(ctx)
}
