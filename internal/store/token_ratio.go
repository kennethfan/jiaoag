package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// TokenRatio 读取某个 `provider/model` 上次留下的估算校准系数。
//
// 没有记录时返回 0，调用方按「没有历史」处理（从默认值起算）。读失败也不该
// 拖住任务，所以这里只返回错误，由调用方决定记日志还是忽略。
func (s *Store) TokenRatio(ctx context.Context, key string) (float64, error) {
	if key == "" {
		return 0, fmt.Errorf("store: 查询估算系数缺少键")
	}
	var ratio float64
	err := s.db.QueryRowContext(ctx,
		`SELECT ratio FROM token_ratios WHERE key = ?`, key).Scan(&ratio)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: 读取估算系数: %w", err)
	}
	return ratio, nil
}

// SaveTokenRatio 写入一个任务结束时的估算系数，并把样本数加一。
//
// samples 只是观测用：知道一个系数是「一次任务猜出来的」还是「几十次校准
// 出来的」，排查预算异常时有用。并发写入取后写者，最后一次为准——系数本身
// 是 EWMA，谁最后落库不影响正确性。
func (s *Store) SaveTokenRatio(ctx context.Context, key string, ratio float64) error {
	if key == "" {
		return fmt.Errorf("store: 保存估算系数缺少键")
	}
	if ratio <= 0 {
		return fmt.Errorf("store: 估算系数必须为正数，得到 %v", ratio)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO token_ratios (key, ratio, samples, updated_at)
		 VALUES (?, ?, 1, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   ratio      = excluded.ratio,
		   samples    = token_ratios.samples + 1,
		   updated_at = excluded.updated_at`,
		key, ratio, ts(domain.Now()))
	if err != nil {
		return fmt.Errorf("store: 保存估算系数: %w", err)
	}
	return nil
}

// TokenRatioSamples 返回某个键已有的校准样本数，供诊断与测试使用。
func (s *Store) TokenRatioSamples(ctx context.Context, key string) (int, error) {
	var samples int
	err := s.db.QueryRowContext(ctx,
		`SELECT samples FROM token_ratios WHERE key = ?`, key).Scan(&samples)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: 读取估算样本数: %w", err)
	}
	return samples, nil
}
