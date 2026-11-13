package app

import (
	"context"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// CheckApprovalConsistency 报告「审批已经裁决、但对应工具调用没有 tool.result」
// 的审批。
//
// 启动时 ReapUnfinished 会把这类调用补成 canceled +「结果未知」，所以正常情况下
// 这里应当为空；它留作安全网：补写失败、或迁移只做了一半时，缺口仍要能查到。
//
// 这类记录意味着收尾那一步丢了：回放出的历史会带着一个没有回填的 tool_use，
// 下一次请求会被供应商直接拒绝。这里只报告、不自动补写——补写要替那次调用
// 选一个 status，而它当时到底跑没跑已经无从判断，猜一个只会把问题藏起来。
func (a *App) CheckApprovalConsistency(ctx context.Context) ([]domain.Approval, error) {
	return a.store.ResolvedApprovalsWithoutToolResult(ctx)
}
