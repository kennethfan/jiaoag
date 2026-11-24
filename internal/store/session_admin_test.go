package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// ListSessionSummaries 是 /sessions 面板的读模型：计数要准，过滤要按工作区。
func TestListSessionSummariesCountsAndFilters(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()

	base := seedSession(t, s) // ses_test @ /tmp/workspace
	second := domain.Session{ID: "ses_second", Workspace: base.Workspace, Title: "第二个"}
	if _, err := s.CreateSession(ctx, second, nil); err != nil {
		t.Fatalf("创建第二个会话: %v", err)
	}
	other := domain.Session{ID: "ses_other", Workspace: "/tmp/other"}
	if _, err := s.CreateSession(ctx, other, nil); err != nil {
		t.Fatalf("创建别的会话: %v", err)
	}

	done := domain.Task{ID: "tsk_done", SessionID: second.ID, Status: domain.TaskQueued, Prompt: "a"}
	if _, err := s.CreateTask(ctx, done, nil); err != nil {
		t.Fatalf("创建任务: %v", err)
	}
	running := domain.Task{ID: "tsk_running", SessionID: second.ID, Status: domain.TaskQueued, Prompt: "b"}
	if _, err := s.CreateTask(ctx, running, nil); err != nil {
		t.Fatalf("创建任务: %v", err)
	}
	// 一个跑完（不计入活跃），一个在跑并带 token 用量。
	if _, err := s.Apply(ctx, domain.Transition{TaskID: done.ID, To: domain.TaskRunning}); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	if _, err := s.Apply(ctx, domain.Transition{TaskID: done.ID, To: domain.TaskCompleted}); err != nil {
		t.Fatalf("置 completed: %v", err)
	}
	if _, err := s.Apply(ctx, domain.Transition{
		TaskID: running.ID,
		To:     domain.TaskRunning,
		Usage:  &domain.TokenUsage{PromptTokens: 1200, CompletionTokens: 340},
	}); err != nil {
		t.Fatalf("置 running 带用量: %v", err)
	}

	same, err := s.ListSessionSummaries(ctx, base.Workspace)
	if err != nil {
		t.Fatalf("列会话统计: %v", err)
	}
	if len(same) != 2 {
		t.Fatalf("同工作区应有 2 条，得到 %d", len(same))
	}
	byID := map[domain.SessionID]domain.SessionSummary{}
	for _, sum := range same {
		byID[sum.ID] = sum
	}
	got, ok := byID[second.ID]
	if !ok {
		t.Fatalf("缺少 %s", second.ID)
	}
	if got.Tasks != 2 || got.ActiveTasks != 1 {
		t.Fatalf("计数不对：tasks=%d active=%d", got.Tasks, got.ActiveTasks)
	}
	if got.PromptTokens != 1200 || got.CompletionTokens != 340 {
		t.Fatalf("token 合计不对：%d/%d", got.PromptTokens, got.CompletionTokens)
	}
	if empty := byID[base.ID]; empty.Tasks != 0 || empty.ActiveTasks != 0 {
		t.Fatalf("空会话计数不对：%+v", empty)
	}

	all, err := s.ListSessionSummaries(ctx, "")
	if err != nil {
		t.Fatalf("列全部会话: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("不过滤应有 3 条，得到 %d", len(all))
	}
}

func TestRenameSessionUpdatesTitleAndEvent(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)

	ev, err := domain.NewEvent(domain.EventSessionRenamed, sess.ID, "",
		domain.SessionRenamedPayload{Title: "新标题"})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	sess.Title = "新标题"
	sess.UpdatedAt = domain.Now()
	stored, err := s.RenameSession(ctx, sess, []domain.Event{ev})
	if err != nil {
		t.Fatalf("改名: %v", err)
	}
	if len(stored) != 1 || stored[0].Type != domain.EventSessionRenamed {
		t.Fatalf("事件不对：%+v", stored)
	}

	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("读会话: %v", err)
	}
	if got.Title != "新标题" {
		t.Fatalf("标题 = %q", got.Title)
	}
	// 没碰 provider/model：改名的输入只有标题。
	if got.Provider != sess.Provider || got.Model != sess.Model {
		t.Fatalf("改名不该动模型：%q/%q", got.Provider, got.Model)
	}

	sess.ID = "ses_missing"
	if _, err := s.RenameSession(ctx, sess, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("改不存在的会话应返回 ErrNotFound，得到 %v", err)
	}
}

// DeleteSession 要清干净：只有 tasks/events 有外键级联，其余四张表得显式处理。
func TestDeleteSessionRemovesRelatedRows(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)

	task := domain.Task{ID: "tsk_del", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "删我"}
	if _, err := s.CreateTask(ctx, task, nil); err != nil {
		t.Fatalf("创建任务: %v", err)
	}
	call := domain.ToolCall{ID: "tc_del", TaskID: task.ID, Name: "write_file", Status: domain.ToolPrepared}
	if _, err := s.Apply(ctx, domain.Transition{
		TaskID:   task.ID,
		To:       domain.TaskRunning,
		ToolCall: &call,
		Approval: &domain.Approval{
			ID: "apr_del", TaskID: task.ID, ToolCallID: call.ID,
			Kind: domain.ApprovalDiff, Risk: domain.RiskMedium, RequestedAt: domain.Now(),
		},
		Proposal: &domain.Proposal{
			ID: "prp_del", TaskID: task.ID, ToolCallID: call.ID, Path: "a.txt",
			Status: domain.ProposalPending,
		},
		Audit: []domain.AuditRecord{{
			Actor: "system", Action: "write.applied", ToolCallID: call.ID, Detail: []byte(`{"path":"a.txt"}`),
		}},
	}); err != nil {
		t.Fatalf("写入工具调用与审批: %v", err)
	}
	if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, To: domain.TaskCompleted}); err != nil {
		t.Fatalf("置 completed: %v", err)
	}
	if err := s.RememberSessionApproval(ctx, string(sess.ID), "k", domain.Now(), domain.Now().Add(time.Hour)); err != nil {
		t.Fatalf("记 allow_session: %v", err)
	}

	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("删除会话: %v", err)
	}

	if _, err := s.GetSession(ctx, sess.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("会话应已删除，得到 %v", err)
	}
	if _, err := s.GetTask(ctx, task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("任务应已删除，得到 %v", err)
	}
	if _, err := s.GetApproval(ctx, "apr_del"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("审批应已删除，得到 %v", err)
	}
	if _, err := s.GetToolCall(ctx, call.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("工具调用应已删除，得到 %v", err)
	}
	audits, err := s.ListAudit(ctx, 0, 10)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(audits) != 0 {
		t.Fatalf("归属该会话的审计应已删除，剩 %d 条", len(audits))
	}
	approved, err := s.SessionApproval(ctx, string(sess.ID), "k", domain.Now())
	if err != nil {
		t.Fatalf("读 allow_session: %v", err)
	}
	if approved {
		t.Fatal("会话授权应随会话删除")
	}
}

func TestDeleteSessionRefusesActiveTask(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	sess := seedSession(t, s)

	task := domain.Task{ID: "tsk_busy", SessionID: sess.ID, Status: domain.TaskQueued, Prompt: "跑着呢"}
	if _, err := s.CreateTask(ctx, task, nil); err != nil {
		t.Fatalf("创建任务: %v", err)
	}
	if _, err := s.Apply(ctx, domain.Transition{TaskID: task.ID, To: domain.TaskRunning}); err != nil {
		t.Fatalf("置 running: %v", err)
	}

	err := s.DeleteSession(ctx, sess.ID)
	if !errors.Is(err, domain.ErrSessionBusy) {
		t.Fatalf("有未结束任务时应拒绝，得到 %v", err)
	}
	if _, err := s.GetSession(ctx, sess.ID); err != nil {
		t.Fatalf("拒绝删除时会话应还在：%v", err)
	}
	if err := s.DeleteSession(ctx, "ses_missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("删不存在的会话应返回 ErrNotFound，得到 %v", err)
	}
}
