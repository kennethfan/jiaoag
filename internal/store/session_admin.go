package store

import (
	"context"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// activeTaskStatuses 是「会话正忙」的判定：排队、执行、等审批。
//
// 三者都会在之后继续写事件或改投影，所以会话在这期间既不能切走也不能删。
var activeTaskStatuses = []any{
	string(domain.TaskQueued),
	string(domain.TaskRunning),
	string(domain.TaskWaitingApproval),
}

// ListSessionSummaries 按最近活动倒序列出会话及其任务/token 计数。
//
// workspace 为空表示不过滤。计数用子查询：单用户场景会话数很少，一次扫描
// 换来「面板一屏就能判断哪条该留」比建索引更划算。
func (s *Store) ListSessionSummaries(ctx context.Context, workspace string) ([]domain.SessionSummary, error) {
	query := `SELECT ` + sessionColumns + `,
       (SELECT count(*) FROM tasks t WHERE t.session_id = sessions.id),
       (SELECT count(*) FROM tasks t WHERE t.session_id = sessions.id AND t.status IN (?, ?, ?)),
       (SELECT coalesce(sum(t.prompt_tokens), 0) FROM tasks t WHERE t.session_id = sessions.id),
       (SELECT coalesce(sum(t.completion_tokens), 0) FROM tasks t WHERE t.session_id = sessions.id)
FROM sessions`
	args := append([]any{}, activeTaskStatuses...)
	if workspace != "" {
		query += ` WHERE workspace = ?`
		args = append(args, workspace)
	}
	query += ` ORDER BY updated_at DESC, id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 列出会话统计: %w", err)
	}
	defer rows.Close()

	var out []domain.SessionSummary
	for rows.Next() {
		var sum domain.SessionSummary
		sess, err := scanSession(rows, &sum.Tasks, &sum.ActiveTasks, &sum.PromptTokens, &sum.CompletionTokens)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描会话统计: %w", err)
		}
		sum.Session = sess
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话统计: %w", err)
	}
	return out, nil
}

// RenameSession 更新会话标题并刷新 updated_at，事件与写入同事务。
//
// 单独一个方法而不是复用 UpdateSession：后者会把 provider/model 一起写回，
// 改名时手里只有标题，拿旧会话回写容易把期间切换过的模型覆盖掉。
func (s *Store) RenameSession(ctx context.Context, sess domain.Session, events []domain.Event) ([]domain.Event, error) {
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

	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = domain.Now()
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET title=?, updated_at=? WHERE id=?`,
		sess.Title, ts(sess.UpdatedAt), string(sess.ID))
	if err != nil {
		return nil, fmt.Errorf("store: 改会话标题 %s: %w", sess.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, fmt.Errorf("store: 会话 %s: %w", sess.ID, domain.ErrNotFound)
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

// DeleteSession 永久删除一个会话及其全部关联数据。
//
// 顺序是先清没有外键的旁支（审计、审批、提案、工具调用、会话授权），再删
// tasks/events/sessions。只有 tasks 与 events 声明了 ON DELETE CASCADE，
// 其余四张表如果只删会话会留下指向不存在会话的孤儿行——那种行既没法追责
// 也没法清理，比删掉更糟。
//
// 有未终态任务时直接拒绝（domain.ErrSessionBusy）：agent 还在跑，删掉会话
// 就没有地方写它的事件了。
func (s *Store) DeleteSession(ctx context.Context, id domain.SessionID) error {
	if !id.Valid() {
		return fmt.Errorf("store: 会话 id 为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE session_id = ? AND status IN (?, ?, ?)`,
		string(id), activeTaskStatuses[0], activeTaskStatuses[1], activeTaskStatuses[2],
	).Scan(&active); err != nil {
		return fmt.Errorf("store: 检查会话 %s 的任务: %w", id, err)
	}
	if active > 0 {
		return fmt.Errorf("store: 会话 %s 还有 %d 个未结束任务: %w", id, active, domain.ErrSessionBusy)
	}

	// 审计没有 session_id 列，只能经 tool_calls 反查归属。context.compact
	// 这类没有工具调用的审计本来就无法归属到会话，删除后仍会留在库里——
	// 这是既有的观测缺口，不在删除里假装处理。
	statements := []struct {
		sql string
		arg any
	}{
		{`DELETE FROM audit WHERE tool_call_id IN (SELECT id FROM tool_calls WHERE session_id = ?)`, string(id)},
		{`DELETE FROM approvals WHERE task_id IN (SELECT id FROM tasks WHERE session_id = ?)`, string(id)},
		{`DELETE FROM proposals WHERE task_id IN (SELECT id FROM tasks WHERE session_id = ?)`, string(id)},
		{`DELETE FROM tool_calls WHERE session_id = ?`, string(id)},
		{`DELETE FROM session_approvals WHERE session_id = ?`, string(id)},
		{`DELETE FROM tasks WHERE session_id = ?`, string(id)},
		{`DELETE FROM events WHERE session_id = ?`, string(id)},
	}
	for _, st := range statements {
		if _, err := tx.ExecContext(ctx, st.sql, st.arg); err != nil {
			return fmt.Errorf("store: 删除会话 %s 的关联数据: %w", id, err)
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("store: 删除会话 %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 删除会话 %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: 会话 %s: %w", id, domain.ErrNotFound)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交会话 %s: %w", id, err)
	}
	return nil
}
