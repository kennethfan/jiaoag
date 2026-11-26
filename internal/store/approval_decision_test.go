package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/store"
)

// seedApproval 建一个任务与一条待裁决审批，供 CAS 用例使用。
func seedApproval(t *testing.T, db *store.Store) (domain.TaskID, domain.ApprovalID) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.CreateSession(ctx, domain.Session{ID: "ses_cas", Workspace: t.TempDir()}, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task := domain.Task{ID: "tsk_cas", SessionID: "ses_cas", Status: domain.TaskQueued, Prompt: "写文件"}
	if _, err := db.CreateTask(ctx, task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	approval := domain.Approval{
		ID:          "apr_cas",
		TaskID:      task.ID,
		Kind:        domain.ApprovalDiff,
		Risk:        domain.RiskMedium,
		RequestedAt: domain.Now(),
	}
	if _, err := db.Apply(ctx, domain.Transition{TaskID: task.ID, Approval: &approval}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return task.ID, approval.ID
}

// 第一次裁决成功、第二次失败；块号与备注一并落库，读回来还是同一份。
func TestResolveApprovalDecisionIsCAS(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	_, id := seedApproval(t, db)

	ok, err := db.ResolveApprovalDecision(ctx, id, domain.DecisionAllowOnce,
		domain.SourceUser, "只批第二块", domain.Now(), []int{1})
	if err != nil || !ok {
		t.Fatalf("第一次裁决 = %v, %v；期望成功", ok, err)
	}
	again, err := db.ResolveApprovalDecision(ctx, id, domain.DecisionDeny,
		domain.SourceUser, "", domain.Now(), nil)
	if err != nil {
		t.Fatalf("第二次裁决报错: %v", err)
	}
	if again {
		t.Fatal("第二次裁决不应该成功")
	}

	approval, err := db.GetApproval(ctx, id)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if approval.Decision != domain.DecisionAllowOnce {
		t.Fatalf("裁决被覆盖成 %q", approval.Decision)
	}
	if approval.Note != "只批第二块" {
		t.Fatalf("备注 = %q", approval.Note)
	}
	if len(approval.Hunks) != 1 || approval.Hunks[0] != 1 {
		t.Fatalf("块号 = %v，期望 [1]", approval.Hunks)
	}
	if approval.Source != domain.SourceUser {
		t.Fatalf("来源 = %q", approval.Source)
	}
	if approval.ResolvedAt.IsZero() {
		t.Fatal("resolved_at 没有落库")
	}
}

// 未裁决时 GetApproval 读出的 decision 为空、块号为空，轮询才能区分
// 「还没人批」与「批了整份」。
func TestPendingApprovalHasNoHunks(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "jiaoag.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	_, id := seedApproval(t, db)

	approval, err := db.GetApproval(ctx, id)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if approval.Decision != "" || approval.Hunks != nil {
		t.Fatalf("待裁决审批 = %+v", approval)
	}
}
