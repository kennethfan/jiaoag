package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// Apply 原子地应用一次状态迁移：校验、更新投影、写审计、追加事件。
//
// 任何一步失败都会整体回滚——不允许出现「有事件但没有记录」或反之。
func (s *Store) Apply(ctx context.Context, tr domain.Transition) ([]domain.Event, error) {
	if err := tr.Validate(); err != nil {
		return nil, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()

	task, err := loadTaskTx(ctx, tx, tr.TaskID)
	if err != nil {
		return nil, err
	}

	if tr.To != "" {
		// 时间戳语义只在 domain.Task.TransitionTo 里维护一份，避免两处漂移。
		if err := task.TransitionTo(tr.To); err != nil {
			return nil, err
		}
		if tr.To == domain.TaskFailed {
			task.Error = tr.Reason
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, started_at = ?, finished_at = ?, error = ? WHERE id = ?`,
			string(task.Status), ts(task.StartedAt), ts(task.FinishedAt), task.Error, string(task.ID)); err != nil {
			return nil, fmt.Errorf("store: 更新任务 %s 状态: %w", task.ID, err)
		}
	}

	if tr.ToolCall != nil {
		if err := upsertToolCall(ctx, tx, *tr.ToolCall, task); err != nil {
			return nil, err
		}
	}
	if tr.Usage != nil {
		// token 是累加量：一次迁移只携带本轮的增量，不覆盖历史累计。
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ? WHERE id = ?`,
			tr.Usage.PromptTokens, tr.Usage.CompletionTokens, string(task.ID)); err != nil {
			return nil, fmt.Errorf("store: 累加任务 %s 的 token: %w", task.ID, err)
		}
	}
	if tr.Approval != nil {
		if err := upsertApproval(ctx, tx, *tr.Approval); err != nil {
			return nil, err
		}
	}
	if tr.Proposal != nil {
		if err := upsertProposal(ctx, tx, *tr.Proposal); err != nil {
			return nil, err
		}
	}
	for _, record := range tr.Audit {
		if err := insertAudit(ctx, tx, record); err != nil {
			return nil, err
		}
	}

	events := withTaskID(tr.Events, task.ID)
	stored, err := appendEventsTx(ctx, tx, task.SessionID, events)
	if err != nil {
		return nil, err
	}
	if len(stored) > 0 {
		last := stored[len(stored)-1].Seq
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET last_seq = ? WHERE id = ?`,
			int64(last), string(task.ID)); err != nil {
			return nil, fmt.Errorf("store: 更新任务 %s 序号: %w", task.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`,
		ts(domain.Now()), string(task.SessionID)); err != nil {
		return nil, fmt.Errorf("store: 更新会话时间: %w", err)
	}

	if s.hookBeforeCommit != nil {
		if err := s.hookBeforeCommit(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交任务 %s 迁移: %w", task.ID, err)
	}
	return stored, nil
}

func loadTaskTx(ctx context.Context, tx *sql.Tx, id domain.TaskID) (domain.Task, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, string(id))
	task, err := scanTask(row)
	if err == sql.ErrNoRows {
		return domain.Task{}, fmt.Errorf("store: 任务 %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Task{}, fmt.Errorf("store: 读取任务 %s: %w", id, err)
	}
	return task, nil
}

func upsertToolCall(ctx context.Context, tx *sql.Tx, call domain.ToolCall, task domain.Task) error {
	if !call.ID.Valid() {
		return fmt.Errorf("store: 工具调用 id 为空")
	}
	if call.TaskID == "" {
		call.TaskID = task.ID
	}
	if call.SessionID == "" {
		call.SessionID = task.SessionID
	}
	if call.CreatedAt.IsZero() {
		call.CreatedAt = domain.Now()
	}
	var exitCode any
	if call.ExitCode != nil {
		exitCode = *call.ExitCode
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tool_calls(id, task_id, session_id, round, name, args, status, approval_id,
		 output_path, exit_code, error, created_at, ended_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   round = excluded.round, name = excluded.name, args = excluded.args,
		   status = excluded.status, approval_id = excluded.approval_id,
		   output_path = excluded.output_path, exit_code = excluded.exit_code,
		   error = excluded.error, ended_at = excluded.ended_at`,
		string(call.ID), string(call.TaskID), string(call.SessionID), call.Round, call.Name,
		string(call.Args), string(call.Status), string(call.ApprovalID), call.OutputPath,
		exitCode, call.Error, ts(call.CreatedAt), ts(call.EndedAt)); err != nil {
		return fmt.Errorf("store: 写入工具调用 %s: %w", call.ID, err)
	}
	return nil
}

func upsertApproval(ctx context.Context, tx *sql.Tx, a domain.Approval) error {
	if !a.ID.Valid() {
		return fmt.Errorf("store: 审批 id 为空")
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = domain.Now()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO approvals(id, task_id, tool_call_id, kind, risk, decision, source,
		 requested_at, resolved_at, note)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   kind = excluded.kind, risk = excluded.risk, decision = excluded.decision,
		   source = excluded.source, resolved_at = excluded.resolved_at, note = excluded.note`,
		string(a.ID), string(a.TaskID), string(a.ToolCallID), string(a.Kind), string(a.Risk),
		string(a.Decision), string(a.Source), ts(a.RequestedAt), ts(a.ResolvedAt), a.Note); err != nil {
		return fmt.Errorf("store: 写入审批 %s: %w", a.ID, err)
	}
	return nil
}

func upsertProposal(ctx context.Context, tx *sql.Tx, p domain.Proposal) error {
	if !p.ID.Valid() {
		return fmt.Errorf("store: 变更提案 id 为空")
	}
	if p.Status == "" {
		p.Status = domain.ProposalPending
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proposals(id, task_id, tool_call_id, path, diff, base_hash, new_hash, status, applied_at)
		 VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   diff = excluded.diff, base_hash = excluded.base_hash, new_hash = excluded.new_hash,
		   status = excluded.status, applied_at = excluded.applied_at`,
		string(p.ID), string(p.TaskID), string(p.ToolCallID), p.Path, p.Diff,
		p.BaseHash, p.NewHash, string(p.Status), ts(p.AppliedAt)); err != nil {
		return fmt.Errorf("store: 写入变更提案 %s: %w", p.ID, err)
	}
	return nil
}

func insertAudit(ctx context.Context, tx *sql.Tx, record domain.AuditRecord) error {
	if record.Action == "" {
		return fmt.Errorf("store: 审计记录缺少 action")
	}
	if record.At.IsZero() {
		record.At = domain.Now()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit(ts, actor, action, tool_call_id, detail) VALUES(?,?,?,?,?)`,
		ts(record.At), record.Actor, record.Action, string(record.ToolCallID), string(record.Detail)); err != nil {
		return fmt.Errorf("store: 写入审计记录 %s: %w", record.Action, err)
	}
	return nil
}
