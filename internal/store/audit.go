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

// ListAudit 按 id 升序返回 afterID 之后的审计记录，供只读消费者增量拉取。
//
// audit 表没有 task_id / session_id 列，这里用 LEFT JOIN 关联出来；没有对应
// 工具调用的记录（例如 context.compact）照样返回，只是归属为空——导出侧
// 不该因为关联不上就丢审计。
func (s *Store) ListAudit(ctx context.Context, afterID int64, limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.ts, a.actor, a.action, COALESCE(a.tool_call_id,''), a.detail,
		        COALESCE(tc.task_id,''), COALESCE(t.session_id,'')
		 FROM audit a
		 LEFT JOIN tool_calls tc ON tc.id = a.tool_call_id
		 LEFT JOIN tasks t ON t.id = tc.task_id
		 WHERE a.id > ? ORDER BY a.id ASC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取审计: %w", err)
	}
	defer rows.Close()

	var out []domain.AuditRecord
	for rows.Next() {
		var (
			record  domain.AuditRecord
			at      string
			actor   string
			action  string
			callID  string
			detail  string
			taskID  string
			session string
		)
		if err := rows.Scan(&record.ID, &at, &actor, &action, &callID, &detail, &taskID, &session); err != nil {
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
		record.TaskID = domain.TaskID(taskID)
		record.SessionID = domain.SessionID(session)
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

// AuditHead 返回当前最大的审计 id；空表返回 0。
func (s *Store) AuditHead(ctx context.Context) (int64, error) {
	var head int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM audit`).Scan(&head); err != nil {
		return 0, fmt.Errorf("store: 读取审计序号: %w", err)
	}
	return head, nil
}
