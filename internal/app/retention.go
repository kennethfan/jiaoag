package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// PruneCandidate 是保留策略可以删掉的会话：所有任务都已终态，且最后活动时间
// 早于保留期。
//
// 粒度刻意定在「会话」而不是「行」：事件是会话回放的唯一来源，单独删 events
// 或 audit 会让历史会话变成残缺状态；整块删掉则每条留痕都完整（ADR-0059）。
type PruneCandidate struct {
	ID        string
	Title     string
	Workspace string
	UpdatedAt time.Time
	Tasks     int
}

// PruneCandidates 列出超过保留期的会话。now 由调用方传入，便于测试与
// 「按某个时间点回看」。
func (a *App) PruneCandidates(ctx context.Context, olderThan time.Duration, now time.Time) ([]PruneCandidate, error) {
	if olderThan <= 0 {
		return nil, fmt.Errorf("app: 保留期必须大于 0")
	}
	summaries, err := a.SessionSummaries(ctx, "")
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-olderThan)
	out := make([]PruneCandidate, 0, len(summaries))
	for _, s := range summaries {
		// 有排队/执行中/等审批的任务一律不动：删除会打断正在干活的东西。
		if s.ActiveTasks > 0 {
			continue
		}
		if !s.UpdatedAt.Before(cutoff) {
			continue
		}
		out = append(out, PruneCandidate{
			ID:        string(s.ID),
			Title:     s.Title,
			Workspace: s.Workspace,
			UpdatedAt: s.UpdatedAt,
			Tasks:     s.Tasks,
		})
	}
	return out, nil
}

// PruneSessions 逐个删除给定会话，返回成功删除的数量。
//
// 删除走的是 DeleteSession：一个事务里按序清掉任务、事件、审计、审批、提案、
// 工具调用与会话授权。删到一半有任务被提交时该会话会报「还有未结束的任务」，
// 这里跳过它继续处理其余会话（不因为一个会话变了就整批失败）。
func (a *App) PruneSessions(ctx context.Context, ids []string) (int, error) {
	deleted := 0
	var errs []error
	for _, id := range ids {
		if err := a.DeleteSession(ctx, domain.SessionID(id)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}
