package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// ServerToken 是库里的一条访问令牌记录。Hash 是令牌明文的 sha256 hex，
// 库里不存明文；RotatedAt 为零值表示从未轮换过。
type ServerToken struct {
	Name      string
	Hash      string
	ReadOnly  bool
	CreatedAt time.Time
	RotatedAt time.Time
	Revoked   bool
}

// ErrTokenExists / ErrTokenNotFound 让 CLI 与调用方能区分「手滑」与「真出错」。
var (
	ErrTokenExists   = errors.New("store: 令牌已存在")
	ErrTokenNotFound = errors.New("store: 令牌不存在或已吊销")
)

const serverTokenColumns = "name, hash, read_only, created_at, rotated_at, revoked_at"

// ServerTokens 列出全部令牌（含已吊销的）。吊销的留在表里是为了能回答
// 「这个令牌什么时候没的」，服务端加载时自己过滤。
func (s *Store) ServerTokens(ctx context.Context) ([]ServerToken, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+serverTokenColumns+" FROM server_tokens ORDER BY name ASC")
	if err != nil {
		return nil, fmt.Errorf("列出访问令牌失败: %w", err)
	}
	defer rows.Close()

	var out []ServerToken
	for rows.Next() {
		var (
			tok      ServerToken
			readOnly int
			created  string
			rotated  string
			revoked  string
		)
		if err := rows.Scan(&tok.Name, &tok.Hash, &readOnly, &created, &rotated, &revoked); err != nil {
			return nil, fmt.Errorf("读取访问令牌失败: %w", err)
		}
		tok.ReadOnly = readOnly != 0
		tok.Revoked = revoked != ""
		if tok.CreatedAt, err = parseTS(created); err != nil {
			return nil, fmt.Errorf("令牌 %s 的创建时间损坏: %w", tok.Name, err)
		}
		if rotated != "" {
			if tok.RotatedAt, err = parseTS(rotated); err != nil {
				return nil, fmt.Errorf("令牌 %s 的轮换时间损坏: %w", tok.Name, err)
			}
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取访问令牌失败: %w", err)
	}
	return out, nil
}

// CreateServerToken 新增一条令牌。名重复时返回 ErrTokenExists。
func (s *Store) CreateServerToken(ctx context.Context, name, hash string, readOnly bool) error {
	name, hash = strings.TrimSpace(name), strings.TrimSpace(hash)
	if name == "" || hash == "" {
		return errors.New("令牌名与哈希都不能为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("创建令牌失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		"INSERT INTO server_tokens (name, hash, read_only, created_at) VALUES (?, ?, ?, ?)",
		name, hash, boolInt(readOnly), ts(domain.Now()))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: %s", ErrTokenExists, name)
		}
		return fmt.Errorf("创建令牌失败: %w", err)
	}
	if err := bumpTokenVersionTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateServerToken 换掉一条令牌的明文：哈希更新，旧值立刻失效。
func (s *Store) RotateServerToken(ctx context.Context, name, hash string) error {
	name, hash = strings.TrimSpace(name), strings.TrimSpace(hash)
	if name == "" || hash == "" {
		return errors.New("令牌名与哈希都不能为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("轮换令牌失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		"UPDATE server_tokens SET hash = ?, rotated_at = ? WHERE name = ? AND revoked_at = ''",
		hash, ts(domain.Now()), name)
	if err != nil {
		return fmt.Errorf("轮换令牌失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("轮换令牌失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTokenNotFound, name)
	}
	if err := bumpTokenVersionTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeServerToken 吊销一条令牌。重复吊销返回 ErrTokenNotFound。
func (s *Store) RevokeServerToken(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("令牌名不能为空")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("吊销令牌失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		"UPDATE server_tokens SET revoked_at = ? WHERE name = ? AND revoked_at = ''",
		ts(domain.Now()), name)
	if err != nil {
		return fmt.Errorf("吊销令牌失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("吊销令牌失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTokenNotFound, name)
	}
	if err := bumpTokenVersionTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ServerTokenVersion 是令牌集合的版本号：任何增删改都会加一，服务端靠它
// 判断是否需要重载（比每次比对全表便宜，也不受时钟影响）。
func (s *Store) ServerTokenVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, "SELECT version FROM server_token_meta WHERE id = 1").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取令牌版本失败: %w", err)
	}
	return v, nil
}

func bumpTokenVersionTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		"UPDATE server_token_meta SET version = version + 1 WHERE id = 1"); err != nil {
		return fmt.Errorf("更新令牌版本失败: %w", err)
	}
	return nil
}
