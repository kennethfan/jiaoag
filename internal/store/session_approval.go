package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionApproval 判断某个会话是否已放行过这组「工具 + 参数原文」。
//
// 过期记录按不存在处理：不靠后台清理保证正确性，否则一次漏跑清理就会
// 让过期的豁免继续生效。
func (s *Store) SessionApproval(ctx context.Context, sessionID, key string, now time.Time) (bool, error) {
	if sessionID == "" || key == "" {
		return false, fmt.Errorf("store: 查询会话授权缺少会话或键")
	}
	var expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT expires_at FROM session_approvals WHERE session_id = ? AND key = ?`,
		sessionID, key).Scan(&expiresAt)
	if err != nil {
		// sql.ErrNoRows 也算「没有授权」，调用方只关心是/否。
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: 读取会话授权: %w", err)
	}
	expires, err := parseTS(expiresAt)
	if err != nil {
		return false, fmt.Errorf("store: 会话授权的时间损坏: %w", err)
	}
	return expires.After(now), nil
}

// RememberSessionApproval 记录一次 allow_session，并顺带清掉已过期的记录。
//
// 清理是顺手做的：这类记录只增不减会慢慢攒出无用行，而按 expires_at 删除
// 比开一个后台任务简单得多。
func (s *Store) RememberSessionApproval(ctx context.Context, sessionID, key string, now, expiresAt time.Time) error {
	if sessionID == "" || key == "" {
		return fmt.Errorf("store: 记录会话授权缺少会话或键")
	}
	if !expiresAt.After(now) {
		return fmt.Errorf("store: 会话授权的过期时间必须晚于当前时间")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM session_approvals WHERE expires_at <= ?`, ts(now)); err != nil {
		return fmt.Errorf("store: 清理过期会话授权: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO session_approvals(session_id, key, created_at, expires_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(session_id, key) DO UPDATE SET created_at = excluded.created_at, expires_at = excluded.expires_at`,
		sessionID, key, ts(now), ts(expiresAt)); err != nil {
		return fmt.Errorf("store: 写入会话授权: %w", err)
	}
	return nil
}

// PruneSessionApprovals 删除已过期的授权记录，返回删除条数。
func (s *Store) PruneSessionApprovals(ctx context.Context, now time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM session_approvals WHERE expires_at <= ?`, ts(now))
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期会话授权: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
