package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// Runtime 是一条运行时注册记录：同一个数据目录下可能同时跑着内嵌 TUI 与
// serve，靠心跳判断对方是否还活着。
type Runtime struct {
	ID          string
	PID         int
	Host        string
	Kind        string
	StartedAt   time.Time
	HeartbeatAt time.Time
}

// RegisterRuntime 写入或更新自己的运行时记录。
func (s *Store) RegisterRuntime(ctx context.Context, rt Runtime) error {
	if rt.ID == "" {
		return fmt.Errorf("store: 运行时 id 为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO runtimes(id, pid, host, kind, started_at, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET heartbeat_at = excluded.heartbeat_at,
                              pid = excluded.pid`,
		rt.ID, rt.PID, rt.Host, rt.Kind, rt.StartedAt.Unix(), rt.HeartbeatAt.Unix())
	if err != nil {
		return fmt.Errorf("store: 注册运行时: %w", err)
	}
	return nil
}

// HeartbeatRuntime 刷新自己的心跳。
func (s *Store) HeartbeatRuntime(ctx context.Context, id string, now time.Time) error {
	if id == "" {
		return fmt.Errorf("store: 运行时 id 为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE runtimes SET heartbeat_at = ? WHERE id = ?`, now.Unix(), id)
	if err != nil {
		return fmt.Errorf("store: 刷新心跳: %w", err)
	}
	return nil
}

// RemoveRuntime 在正常退出时删掉记录。
func (s *Store) RemoveRuntime(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtimes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 注销运行时: %w", err)
	}
	return nil
}

// LiveRuntimes 返回 since 之后仍有心跳的运行时。
func (s *Store) LiveRuntimes(ctx context.Context, since time.Time) ([]Runtime, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, pid, host, kind, started_at, heartbeat_at
FROM runtimes WHERE heartbeat_at >= ? ORDER BY id`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: 查询运行时: %w", err)
	}
	defer rows.Close()
	var out []Runtime
	for rows.Next() {
		var (
			rt               Runtime
			started, beatInt int64
		)
		if err := rows.Scan(&rt.ID, &rt.PID, &rt.Host, &rt.Kind, &started, &beatInt); err != nil {
			return nil, fmt.Errorf("store: 读取运行时: %w", err)
		}
		rt.StartedAt = time.Unix(started, 0)
		rt.HeartbeatAt = time.Unix(beatInt, 0)
		out = append(out, rt)
	}
	return out, rows.Err()
}

// ClaimTask 尝试为一条 queued 任务占位。返回 false 表示这一轮抢不到
// （任务已被别人抢走、会话仍有活任务、或全局并发已满），调用方应当稍后重试。
//
// 三条约束都在同一条 UPDATE 里完成，SQLite 的单写者语义保证原子：
//   - 同一会话同时只能有一条 running/waiting_approval 或已占位（owner 非空且租约未过期）的任务；
//   - 全局并发不超过 maxRunning（<=0 表示不限）；
//   - 只能抢 owner 仍为空的 queued 任务。
func (s *Store) ClaimTask(ctx context.Context, id domain.TaskID, owner string, until time.Time, maxRunning int) (bool, error) {
	if id == "" || owner == "" {
		return false, fmt.Errorf("store: 占位参数为空")
	}
	now := time.Now().Unix()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `
UPDATE tasks SET owner = ?, lease_until = ?
WHERE id = ? AND status = 'queued' AND owner = ''
  AND NOT EXISTS (
      SELECT 1 FROM tasks t2
       WHERE t2.session_id = tasks.session_id AND t2.id <> tasks.id
         AND (t2.status IN ('running','waiting_approval')
              OR (t2.owner <> '' AND t2.lease_until >= ?)))
  AND (? <= 0 OR (SELECT COUNT(*) FROM tasks
                   WHERE status = 'running'
                      OR (owner <> '' AND lease_until >= ?)) < ?)`,
		owner, until.Unix(), string(id), now, maxRunning, now, maxRunning)
	if err != nil {
		return false, fmt.Errorf("store: 占位任务 %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 占位结果: %w", err)
	}
	return n == 1, nil
}

// RenewTaskLeases 续租该 owner 名下所有未结束任务（含等待审批的任务）。
// 返回续租条数，便于日志与测试。
func (s *Store) RenewTaskLeases(ctx context.Context, owner string, until time.Time) (int64, error) {
	if owner == "" {
		return 0, fmt.Errorf("store: owner 为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx, `
UPDATE tasks SET lease_until = ?
WHERE owner = ? AND status IN ('queued','running','waiting_approval')`, until.Unix(), owner)
	if err != nil {
		return 0, fmt.Errorf("store: 续租: %w", err)
	}
	return res.RowsAffected()
}

// ExpiredTasks 返回租约已过期、但仍占着位置的任务。调用方负责补终态：
// 只标记，不重新入队——重跑一个可能已经产生副作用的任务比失败更危险。
func (s *Store) ExpiredTasks(ctx context.Context, now time.Time) ([]domain.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, status, owner FROM tasks
WHERE status IN ('queued','running','waiting_approval')
  AND owner <> '' AND lease_until < ?
ORDER BY created_at`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: 查询过期租约: %w", err)
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		var t domain.Task
		var id, sid string
		if err := rows.Scan(&id, &sid, &t.Status, &t.Owner); err != nil {
			return nil, fmt.Errorf("store: 读取过期租约: %w", err)
		}
		t.ID = domain.TaskID(id)
		t.SessionID = domain.SessionID(sid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReleaseTaskClaim 把还没真正启动的任务还回队列（owner 清空），供下一次抢占。
func (s *Store) ReleaseTaskClaim(ctx context.Context, id domain.TaskID, owner string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET owner = '', lease_until = 0 WHERE id = ? AND owner = ? AND status = 'queued'`,
		string(id), owner)
	if err != nil {
		return fmt.Errorf("store: 归还任务占位: %w", err)
	}
	return nil
}

// TaskOwner 返回任务的占用者（空表示没人占）。
func (s *Store) TaskOwner(ctx context.Context, id domain.TaskID) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM tasks WHERE id = ?`, string(id)).Scan(&owner)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("store: 查询任务归属: %w", err)
	}
	return owner, nil
}
