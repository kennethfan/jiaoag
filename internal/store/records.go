package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// CreateSession 原子地写入会话与其事件。
func (s *Store) CreateSession(ctx context.Context, sess domain.Session, events []domain.Event) ([]domain.Event, error) {
	if !sess.ID.Valid() {
		return nil, fmt.Errorf("store: 会话 id 为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()

	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = domain.Now()
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = sess.CreatedAt
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions(id, workspace, provider, model, title, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		string(sess.ID), sess.Workspace, sess.Provider, sess.Model, sess.Title,
		ts(sess.CreatedAt), ts(sess.UpdatedAt)); err != nil {
		return nil, fmt.Errorf("store: 写入会话 %s: %w", sess.ID, err)
	}

	stored, err := appendEventsTx(ctx, tx, sess.ID, events)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交会话 %s: %w", sess.ID, err)
	}
	return stored, nil
}

const sessionColumns = `id, workspace, provider, model, title, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (domain.Session, error) {
	var (
		sess             domain.Session
		id               string
		created, updated string
	)
	if err := row.Scan(&id, &sess.Workspace, &sess.Provider, &sess.Model, &sess.Title, &created, &updated); err != nil {
		return domain.Session{}, err
	}
	sess.ID = domain.SessionID(id)
	var err error
	if sess.CreatedAt, err = parseTS(created); err != nil {
		return domain.Session{}, err
	}
	if sess.UpdatedAt, err = parseTS(updated); err != nil {
		return domain.Session{}, err
	}
	return sess, nil
}

// GetSession 按标识取会话。
func (s *Store) GetSession(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, string(id))
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Session{}, fmt.Errorf("store: 会话 %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("store: 读取会话 %s: %w", id, err)
	}
	return sess, nil
}

// ListSessions 按最近活动倒序列出会话。
func (s *Store) ListSessions(ctx context.Context) ([]domain.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY updated_at DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出会话: %w", err)
	}
	defer rows.Close()

	var out []domain.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描会话: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话: %w", err)
	}
	return out, nil
}

// CreateTask 原子地写入任务与其事件（通常是 task.queued）。
func (s *Store) CreateTask(ctx context.Context, task domain.Task, events []domain.Event) ([]domain.Event, error) {
	if !task.ID.Valid() {
		return nil, fmt.Errorf("store: 任务 id 为空")
	}
	if !task.SessionID.Valid() {
		return nil, fmt.Errorf("store: 任务 %s 缺少会话", task.ID)
	}
	if task.Status == "" {
		task.Status = domain.TaskQueued
	}
	if !task.Status.Valid() {
		return nil, fmt.Errorf("store: 任务 %s 状态非法: %q", task.ID, task.Status)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = domain.Now()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(id, session_id, status, prompt, system_prompt_hash, extensions_digest,
		 last_seq, created_at, started_at, finished_at, error, prompt_tokens, completion_tokens)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(task.ID), string(task.SessionID), string(task.Status), task.Prompt,
		task.SystemPromptHash, task.ExtensionsDigest, int64(task.LastSeq),
		ts(task.CreatedAt), ts(task.StartedAt), ts(task.FinishedAt), task.Error,
		task.PromptTokens, task.CompleteTokens); err != nil {
		return nil, fmt.Errorf("store: 写入任务 %s: %w", task.ID, err)
	}

	stored, err := appendEventsTx(ctx, tx, task.SessionID, withTaskID(events, task.ID))
	if err != nil {
		return nil, err
	}
	if len(stored) > 0 {
		last := stored[len(stored)-1].Seq
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET last_seq = ? WHERE id = ?`, int64(last), string(task.ID)); err != nil {
			return nil, fmt.Errorf("store: 更新任务 %s 序号: %w", task.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`,
		ts(domain.Now()), string(task.SessionID)); err != nil {
		return nil, fmt.Errorf("store: 更新会话时间: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交任务 %s: %w", task.ID, err)
	}
	return stored, nil
}

func withTaskID(events []domain.Event, id domain.TaskID) []domain.Event {
	out := make([]domain.Event, len(events))
	copy(out, events)
	for i := range out {
		if out[i].TaskID == "" {
			out[i].TaskID = id
		}
	}
	return out
}

const taskColumns = `id, session_id, status, prompt, system_prompt_hash, extensions_digest,
	last_seq, created_at, started_at, finished_at, error, prompt_tokens, completion_tokens`

func scanTask(row interface{ Scan(...any) error }) (domain.Task, error) {
	var (
		task                       domain.Task
		id, session, status        string
		lastSeq                    int64
		created, started, finished string
	)
	if err := row.Scan(&id, &session, &status, &task.Prompt, &task.SystemPromptHash,
		&task.ExtensionsDigest, &lastSeq, &created, &started, &finished, &task.Error,
		&task.PromptTokens, &task.CompleteTokens); err != nil {
		return domain.Task{}, err
	}
	task.ID = domain.TaskID(id)
	task.SessionID = domain.SessionID(session)
	task.Status = domain.TaskStatus(status)
	task.LastSeq = domain.Seq(lastSeq)
	var err error
	if task.CreatedAt, err = parseTS(created); err != nil {
		return domain.Task{}, err
	}
	if task.StartedAt, err = parseTS(started); err != nil {
		return domain.Task{}, err
	}
	if task.FinishedAt, err = parseTS(finished); err != nil {
		return domain.Task{}, err
	}
	return task, nil
}

// GetTask 按标识取任务。
func (s *Store) GetTask(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, string(id))
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, fmt.Errorf("store: 任务 %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Task{}, fmt.Errorf("store: 读取任务 %s: %w", id, err)
	}
	return task, nil
}

// ListTasks 按创建时间正序列出会话内任务。
func (s *Store) ListTasks(ctx context.Context, sessionID domain.SessionID) ([]domain.Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE session_id = ? ORDER BY created_at ASC, id ASC`, string(sessionID))
	if err != nil {
		return nil, fmt.Errorf("store: 列出任务: %w", err)
	}
	defer rows.Close()

	var out []domain.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描任务: %w", err)
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历任务: %w", err)
	}
	return out, nil
}

// Replay 返回 Seq 严格大于 from 的事件，按 Seq 升序。
func (s *Store) Replay(ctx context.Context, sessionID domain.SessionID, from domain.Seq, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, seq, v, type, payload, created_at, task_id
		 FROM events WHERE session_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		string(sessionID), int64(from), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 回放会话 %s: %w", sessionID, err)
	}
	return decodeEvents(rows)
}

// LastSeq 返回会话内最后一个已分配的事件序号。
func (s *Store) LastSeq(ctx context.Context, sessionID domain.SessionID) (domain.Seq, error) {
	var last int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id = ?`, string(sessionID)).Scan(&last); err != nil {
		return 0, fmt.Errorf("store: 读取会话 %s 最后序号: %w", sessionID, err)
	}
	return domain.Seq(last), nil
}

// GetApproval 按标识读取审批。不存在时返回 domain.ErrNotFound。
func (s *Store) GetApproval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, tool_call_id, kind, risk, decision, source, requested_at, resolved_at, note
		 FROM approvals WHERE id = ?`, string(id))
	var (
		a                   domain.Approval
		rawID, task, call   string
		kind, risk          string
		decision, source    string
		note                string
		requested, resolved string
	)
	if err := row.Scan(&rawID, &task, &call, &kind, &risk, &decision, &source,
		&requested, &resolved, &note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Approval{}, fmt.Errorf("store: 审批 %s: %w", id, domain.ErrNotFound)
		}
		return domain.Approval{}, fmt.Errorf("store: 读取审批 %s: %w", id, err)
	}
	a.ID = domain.ApprovalID(rawID)
	a.TaskID = domain.TaskID(task)
	a.ToolCallID = domain.ToolCallID(call)
	a.Kind = domain.ApprovalKind(kind)
	a.Risk = domain.Risk(risk)
	a.Decision = domain.Decision(decision)
	a.Source = domain.ApprovalSource(source)
	a.Note = note
	var err error
	if a.RequestedAt, err = parseTS(requested); err != nil {
		return domain.Approval{}, err
	}
	if a.ResolvedAt, err = parseTS(resolved); err != nil {
		return domain.Approval{}, err
	}
	return a, nil
}

// GetToolCall 按标识读取工具调用。
//
// 审批裁决要用它取回参数原文：allow_session 的白名单键是 tool+args 的摘要，
// 而 approval 表只存了 tool_call_id。
func (s *Store) GetToolCall(ctx context.Context, id domain.ToolCallID) (domain.ToolCall, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, session_id, round, name, args, status, approval_id,
		 output_path, exit_code, error, created_at, ended_at
		 FROM tool_calls WHERE id = ?`, string(id))
	var (
		tc                         domain.ToolCall
		rawID, task, session, name string
		args, status, approval     string
		outputPath, errText        string
		created, ended             string
		exitCode                   sql.NullInt64
	)
	if err := row.Scan(&rawID, &task, &session, &tc.Round, &name, &args, &status, &approval,
		&outputPath, &exitCode, &errText, &created, &ended); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ToolCall{}, fmt.Errorf("store: 工具调用 %s: %w", id, domain.ErrNotFound)
		}
		return domain.ToolCall{}, fmt.Errorf("store: 读取工具调用 %s: %w", id, err)
	}
	tc.ID = domain.ToolCallID(rawID)
	tc.TaskID = domain.TaskID(task)
	tc.SessionID = domain.SessionID(session)
	tc.Name = name
	tc.Args = json.RawMessage(args)
	tc.Status = domain.ToolCallStatus(status)
	tc.ApprovalID = domain.ApprovalID(approval)
	tc.OutputPath = outputPath
	tc.Error = errText
	if exitCode.Valid {
		code := int(exitCode.Int64)
		tc.ExitCode = &code
	}
	var err error
	if tc.CreatedAt, err = parseTS(created); err != nil {
		return domain.ToolCall{}, err
	}
	if tc.EndedAt, err = parseTS(ended); err != nil {
		return domain.ToolCall{}, err
	}
	return tc, nil
}

// PendingApprovals 返回所有尚未裁决的审批，按请求时间升序。
//
// 只给启动清理用：进程退出时还没来得及裁决的审批，重启后永远不会有人来
// 裁决（v1 不自动续跑），必须显式标成 expired。
func (s *Store) PendingApprovals(ctx context.Context) ([]domain.Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM approvals WHERE decision = '' ORDER BY requested_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取待审批: %w", err)
	}
	var ids []domain.ApprovalID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: 扫描待审批: %w", err)
		}
		ids = append(ids, domain.ApprovalID(id))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: 遍历待审批: %w", err)
	}
	rows.Close()

	out := make([]domain.Approval, 0, len(ids))
	for _, id := range ids {
		a, err := s.GetApproval(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ProposalByToolCall 按工具调用查变更提案。
//
// 一次工具调用最多产生一个提案（写入工具只能提出一次改动），因此按
// tool_call_id 取是安全的。
func (s *Store) ProposalByToolCall(ctx context.Context, id domain.ToolCallID) (domain.Proposal, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, tool_call_id, path, diff, base_hash, new_hash, status, applied_at
		 FROM proposals WHERE tool_call_id = ? ORDER BY id ASC LIMIT 1`, string(id))
	var (
		p                       domain.Proposal
		rawID, task, call, path string
		diff, baseHash, newHash string
		status, applied         string
	)
	if err := row.Scan(&rawID, &task, &call, &path, &diff, &baseHash, &newHash, &status, &applied); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Proposal{}, fmt.Errorf("store: 工具调用 %s 的提案: %w", id, domain.ErrNotFound)
		}
		return domain.Proposal{}, fmt.Errorf("store: 读取工具调用 %s 的提案: %w", id, err)
	}
	p.ID = domain.ProposalID(rawID)
	p.TaskID = domain.TaskID(task)
	p.ToolCallID = domain.ToolCallID(call)
	p.Path = path
	p.Diff = diff
	p.BaseHash = baseHash
	p.NewHash = newHash
	p.Status = domain.ProposalStatus(status)
	var err error
	if p.AppliedAt, err = parseTS(applied); err != nil {
		return domain.Proposal{}, err
	}
	return p, nil
}
