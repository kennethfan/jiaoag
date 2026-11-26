package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BackgroundProc 是后台进程在库里的投影。它只回答「谁在这个会话里起过什么、
// 现在还要不要它跑」，进程句柄仍归启动它的运行时所有。
type BackgroundProc struct {
	ID            string
	SessionID     string
	Owner         string
	PID           int
	OutputPath    string
	StartedAt     time.Time
	Status        string // running | exited | stopped | orphaned
	StopRequested bool
}

// 后台进程状态。orphaned 表示属主运行时已失联：只读展示，不自动杀（pid 可能
// 已经被系统复用，替别人收尸比留一个僵尸更危险）。
const (
	ProcRunning  = "running"
	ProcExited   = "exited"
	ProcStopped  = "stopped"
	ProcOrphaned = "orphaned"
)

// RegisterBackgroundProc 登记刚启动的后台进程。
func (s *Store) RegisterBackgroundProc(ctx context.Context, p BackgroundProc) error {
	if p.ID == "" || p.SessionID == "" {
		return fmt.Errorf("store: 后台进程缺少 id 或会话")
	}
	if p.Status == "" {
		p.Status = ProcRunning
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO background_procs (id, session_id, owner, pid, output_path, started_at, status, stop_requested)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    owner = excluded.owner,
    pid = excluded.pid,
    output_path = excluded.output_path,
    status = excluded.status`,
		p.ID, p.SessionID, p.Owner, p.PID, p.OutputPath, p.StartedAt.Unix(), p.Status, boolInt(p.StopRequested))
	if err != nil {
		return fmt.Errorf("store: 登记后台进程: %w", err)
	}
	return nil
}

// MarkBackgroundProc 更新进程状态（退出 / 已停止 / 孤儿）。
func (s *Store) MarkBackgroundProc(ctx context.Context, id, status string) error {
	if id == "" || status == "" {
		return fmt.Errorf("store: 后台进程状态参数为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE background_procs SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("store: 更新后台进程状态: %w", err)
	}
	return nil
}

// RequestBackgroundProcStop 请属主运行时停掉进程。别的进程只能投递意图，
// 真正 kill 必须由持有句柄的那个运行时执行。
func (s *Store) RequestBackgroundProcStop(ctx context.Context, sessionID, id string) error {
	if id == "" {
		return fmt.Errorf("store: 后台进程 id 为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
UPDATE background_procs SET stop_requested = 1
WHERE id = ? AND session_id = ? AND status = ?`, id, sessionID, ProcRunning)
	if err != nil {
		return fmt.Errorf("store: 请求停止后台进程: %w", err)
	}
	return nil
}

// BackgroundProcStopRequested 供属主运行时轮询。
func (s *Store) BackgroundProcStopRequested(ctx context.Context, id string) (bool, error) {
	var flag int
	err := s.db.QueryRowContext(ctx,
		`SELECT stop_requested FROM background_procs WHERE id = ?`, id).Scan(&flag)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: 读取停止标记: %w", err)
	}
	return flag != 0, nil
}

// ListBackgroundProcs 返回一个会话的后台进程（含别的运行时启动的）。
func (s *Store) ListBackgroundProcs(ctx context.Context, sessionID string) ([]BackgroundProc, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, owner, pid, output_path, started_at, status, stop_requested
FROM background_procs WHERE session_id = ? ORDER BY started_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询后台进程: %w", err)
	}
	defer rows.Close()
	var out []BackgroundProc
	for rows.Next() {
		var (
			p     BackgroundProc
			start int64
			flag  int
		)
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Owner, &p.PID, &p.OutputPath,
			&start, &p.Status, &flag); err != nil {
			return nil, fmt.Errorf("store: 读取后台进程: %w", err)
		}
		p.StartedAt = time.Unix(start, 0)
		p.StopRequested = flag != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// OrphanBackgroundProcs 把属主已经失联的进程标成 orphaned。
func (s *Store) OrphanBackgroundProcs(ctx context.Context, liveOwners []string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	query := `UPDATE background_procs SET status = ? WHERE status = ?`
	args := []any{ProcOrphaned, ProcRunning}
	if len(liveOwners) > 0 {
		query += ` AND owner NOT IN (?` + strings.Repeat(",?", len(liveOwners)-1) + `)`
		for _, id := range liveOwners {
			args = append(args, id)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: 标记孤儿后台进程: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
