package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// ProjectTrust 读取某个工作区已确认的项目扩展摘要。没有记录时返回空串。
func (s *Store) ProjectTrust(ctx context.Context, workspace string) (string, error) {
	var digest string
	err := s.db.QueryRowContext(ctx,
		`SELECT digest FROM project_trust WHERE workspace = ?`, workspace).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: 读取项目信任记录: %w", err)
	}
	return digest, nil
}

// TrustProject 记录（或替换）某个工作区的项目扩展摘要与确认时间。
func (s *Store) TrustProject(ctx context.Context, workspace, digest string) error {
	if workspace == "" || digest == "" {
		return fmt.Errorf("store: 项目信任记录缺少工作区或摘要")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO project_trust(workspace, digest, trusted_at, note)
		 VALUES(?, ?, ?, '')
		 ON CONFLICT(workspace) DO UPDATE SET digest = excluded.digest, trusted_at = excluded.trusted_at`,
		workspace, digest, ts(domain.Now())); err != nil {
		return fmt.Errorf("store: 写入项目信任记录: %w", err)
	}
	return nil
}

// RevokeProjectTrust 撤销某个工作区的项目扩展信任；没有记录时是幂等空操作。
func (s *Store) RevokeProjectTrust(ctx context.Context, workspace string) error {
	if workspace == "" {
		return fmt.Errorf("store: 撤销项目信任记录缺少工作区")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM project_trust WHERE workspace = ?`, workspace); err != nil {
		return fmt.Errorf("store: 撤销项目信任记录: %w", err)
	}
	return nil
}
