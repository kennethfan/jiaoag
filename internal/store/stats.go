package store

import "context"

// Stats 是给 /metrics 用的只读聚合。都走 COUNT，不锁业务行。
type Stats struct {
	Sessions      int
	TasksByStatus map[string]int
	Events        int64
	Audit         int64
	ProcsRunning  int
	ProcsOrphaned int
}

// Stats 汇总库里的行数。会话/任务/进程按状态分开，事件与审计只要总数——
// 指标系统关心的是「有没有在长」，不是每一行。
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	out := Stats{TasksByStatus: map[string]int{}}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&out.Sessions); err != nil {
		return Stats{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return Stats{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status string
			count  int
		)
		if err := rows.Scan(&status, &count); err != nil {
			return Stats{}, err
		}
		out.TasksByStatus[status] = count
	}
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&out.Events); err != nil {
		return Stats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`).Scan(&out.Audit); err != nil {
		return Stats{}, err
	}

	procRows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM background_procs GROUP BY status`)
	if err != nil {
		return Stats{}, err
	}
	defer procRows.Close()
	for procRows.Next() {
		var (
			status string
			count  int
		)
		if err := procRows.Scan(&status, &count); err != nil {
			return Stats{}, err
		}
		switch status {
		case "running":
			out.ProcsRunning = count
		case "orphaned":
			out.ProcsOrphaned = count
		}
	}
	if err := procRows.Err(); err != nil {
		return Stats{}, err
	}
	return out, nil
}
