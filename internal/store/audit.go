package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// RecentAudit 返回某个任务最近的审计记录。
//
// audit 表没有 task_id 列（M1 的 schema 即如此），按 tool_call_id 关联
// tool_calls 取任务。只做最小只读查询：导出、检索语言与完整 UI 留到 v2。
func (s *Store) RecentAudit(ctx context.Context, taskID domain.TaskID, limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.ts, a.actor, a.action, a.tool_call_id, a.detail
		 FROM audit a JOIN tool_calls tc ON tc.id = a.tool_call_id
		 WHERE tc.task_id = ? ORDER BY a.id DESC LIMIT ?`, string(taskID), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取任务 %s 的审计: %w", taskID, err)
	}
	defer rows.Close()

	var out []domain.AuditRecord
	for rows.Next() {
		var (
			record domain.AuditRecord
			at     string
			actor  string
			action string
			callID string
			detail string
		)
		if err := rows.Scan(&record.ID, &at, &actor, &action, &callID, &detail); err != nil {
			return nil, fmt.Errorf("store: 扫描审计: %w", err)
		}
		parsed, err := parseTS(at)
		if err != nil {
			return nil, err
		}
		record.At = parsed
		record.Actor = actor
		record.Action = action
		record.ToolCallID = domain.ToolCallID(callID)
		if detail != "" {
			record.Detail = json.RawMessage(detail)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历审计: %w", err)
	}
	return out, nil
}
