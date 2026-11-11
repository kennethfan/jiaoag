package store

import (
	"context"
	"database/sql"
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
