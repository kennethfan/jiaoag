package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// ResolveApprovalDecision 用 CAS 写入一次裁决：只有第一个写入者成功。
//
// 裁决只有一份事实来源——approvals 表。多运行时下批准可能发生在另一个
// 进程里，等待的协程靠轮询这张表拿到结果（ADR-0050）；因此这里不能用
// 「读一次、判断没裁决、再写」的写法，那会让两个进程都以为自己赢了。
func (s *Store) ResolveApprovalDecision(ctx context.Context, id domain.ApprovalID,
	decision domain.Decision, source domain.ApprovalSource, note string,
	at time.Time, hunks []int) (bool, error) {

	if id == "" {
		return false, fmt.Errorf("store: 审批 id 为空")
	}
	raw, err := hunksColumn(hunks)
	if err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals
		    SET decision = ?, source = ?, resolved_at = ?, note = ?, hunks = ?
		  WHERE id = ? AND decision = ''`,
		string(decision), string(source), ts(at), note, raw, string(id))
	if err != nil {
		return false, fmt.Errorf("store: 写入审批 %s 的裁决: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 写入审批 %s 的裁决: %w", id, err)
	}
	return n > 0, nil
}

// hunksColumn 把块号编码成表里的文本。空切片与 nil 都表示「整份应用」，
// 存空串而不是 "[]"，这样历史行（迁移前写入的）语义一致。
func hunksColumn(hunks []int) (string, error) {
	if len(hunks) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(hunks)
	if err != nil {
		return "", fmt.Errorf("store: 编码审批块号: %w", err)
	}
	return string(raw), nil
}

func parseHunks(raw string) ([]int, error) {
	if raw == "" {
		return nil, nil
	}
	var out []int
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("store: 审批块号损坏: %w", err)
	}
	return out, nil
}
