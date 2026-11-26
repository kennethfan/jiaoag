package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
)

// 两个运行时共用一个库：A 里的任务在等审批，B 的用户批准，A 的协程靠
// 轮询 approvals 表拿到裁决并继续。
//
// 这是 P2 的核心回归：裁决不再投递给进程内的等待者，审批状态必须能从
// 另一个进程写的同一个库里读回来（ADR-0050）。
func TestApprovalResolvedByAnotherRuntime(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "jiaoag.db")
	dir := t.TempDir()

	db1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开库 1: %v", err)
	}
	a1 := app.New(db1, event.NewHub())
	defer a1.Close()

	db2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开库 2: %v", err)
	}
	a2 := app.New(db2, event.NewHub())
	defer a2.Close()

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		writeToolRound("call_cross", "out.txt", "cross-runtime"),
	}}
	configureWriteRuntime(t, a1, provider)

	sess, err := a1.CreateSession(ctx, dir, "", "", "跨运行时审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := a1.SubmitTask(ctx, sess.ID, "把内容写到 out.txt"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a1, sess.ID)
	if err := a2.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, "另一个进程批的"); err != nil {
		t.Fatalf("另一个运行时的 ResolveApproval: %v", err)
	}
	// 重复裁决必须幂等：CAS 只有一个写入者成功，第二个不报错也不改结果。
	if err := a2.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionDeny, ""); err != nil {
		t.Fatalf("重复裁决应当幂等: %v", err)
	}

	done := waitTaskDone(t, a1, sess.ID)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("任务状态 = %s，期望 completed（error=%q）", done.Status, done.Error)
	}
	content, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("读取写入结果: %v", err)
	}
	if string(content) != "cross-runtime" {
		t.Fatalf("文件内容 = %q", content)
	}

	approval, err := db1.GetApproval(ctx, approvalID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if approval.Decision != domain.DecisionAllowOnce || approval.Note != "另一个进程批的" {
		t.Fatalf("审批落库结果 = %+v", approval)
	}
}
