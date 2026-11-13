package app_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// roundProvider 按调用次序回放预置脚本：先用工具、再收尾。
type roundProvider struct {
	rounds []func(fn func(llm.Chunk) error) error
	calls  int
}

func (p *roundProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, os.ErrInvalid
}

func (p *roundProvider) ChatStream(ctx context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if p.calls >= len(p.rounds) {
		return fn(llm.Chunk{TextDelta: "结束"})
	}
	script := p.rounds[p.calls]
	p.calls++
	return script(fn)
}

func writeToolRound(id, path, content string) func(func(llm.Chunk) error) error {
	return func(fn func(llm.Chunk) error) error {
		if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: id, Name: "write_file"}}); err != nil {
			return err
		}
		args := `{"path":"` + path + `","content":"` + content + `"}`
		if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: args}}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopToolUse})
	}
}

func textRound(text string) func(func(llm.Chunk) error) error {
	return func(fn func(llm.Chunk) error) error {
		if err := fn(llm.Chunk{TextDelta: text}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd,
			Usage: &llm.Usage{PromptTokens: 5, CompletionTokens: 2}})
	}
}

func configureWriteRuntime(t *testing.T, a *app.App, provider llm.Provider) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(
				tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
				tools.NewWriteFile(ws), tools.NewEditFile(ws),
			)
		},
		MaxRounds:     4,
		ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// waitApproval 等到会话里出现第一条待审批，并返回它的 id。
func waitApproval(t *testing.T, a *app.App, sessionID domain.SessionID) domain.ApprovalID {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := a.Events(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("读取事件: %v", err)
		}
		for _, ev := range events {
			if ev.Type != domain.EventApprovalRequested {
				continue
			}
			var p domain.ApprovalRequestedPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatalf("解码审批请求: %v", err)
			}
			return p.ApprovalID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("没有等到 approval.requested")
	return ""
}

func TestApprovalFlowWritesFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		writeToolRound("call_w", "out.txt", "approved content"),
		textRound("写好了"),
	}}
	a := newApp(t)
	configureWriteRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "p", "m", "审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "写个文件")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, ""); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	done := waitTerminal(t, a, task.ID)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("读取落盘文件: %v", err)
	}
	if string(got) != "approved content" {
		t.Fatalf("落盘内容 = %q", got)
	}

	events, _ := a.Events(ctx, sess.ID)
	if !hasEvent(events, domain.EventProposalApplied) {
		t.Fatalf("缺少 proposal.applied：%v", kindsOf(events))
	}
}

func TestApprovalDenyLeavesDiskUntouched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		writeToolRound("call_w", "out.txt", "nope"),
		textRound("好的"),
	}}
	a := newApp(t)
	configureWriteRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "p", "m", "拒绝")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "写个文件")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionDeny, "不要"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("拒绝不该让任务失败，终态 = %s（%s）", done.Status, done.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("拒绝后磁盘不该有变化")
	}
	events, _ := a.Events(ctx, sess.ID)
	if !hasEvent(events, domain.EventProposalRejected) {
		t.Fatalf("缺少 proposal.rejected：%v", kindsOf(events))
	}
}

func TestResolveApprovalValidates(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	configureWriteRuntime(t, a, &roundProvider{})
	sess, err := a.CreateSession(ctx, t.TempDir(), "p", "m", "校验")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := a.ResolveApproval(ctx, sess.ID, "apr_missing", domain.DecisionAllowOnce, ""); err == nil {
		t.Fatal("不存在的审批应报错")
	}

	// 造一条待审批记录，验证未知裁决被拒绝。
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approval := &domain.Approval{
		ID: "apr_1", TaskID: task.ID, ToolCallID: "call_x",
		Kind: domain.ApprovalDiff, Risk: domain.RiskMedium, RequestedAt: domain.Now(),
	}
	if err := a.Record(ctx, domain.Transition{TaskID: task.ID, Approval: approval}); err != nil {
		t.Fatalf("写入审批: %v", err)
	}
	if err := a.ResolveApproval(ctx, sess.ID, "apr_1", "maybe", ""); err == nil {
		t.Fatal("未知裁决应被拒绝")
	}
	// 重复裁决是幂等的空操作。
	if err := a.ResolveApproval(ctx, sess.ID, "apr_1", domain.DecisionDeny, "第一次"); err != nil {
		t.Fatalf("第一次裁决: %v", err)
	}
	if err := a.ResolveApproval(ctx, sess.ID, "apr_1", domain.DecisionAllowOnce, ""); err != nil {
		t.Fatalf("重复裁决应是幂等空操作: %v", err)
	}
}

func TestAllowSessionSkipsSecondPrompt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		writeToolRound("call_w1", "out.txt", "same content"),
		textRound("第一次完成"),
		writeToolRound("call_w2", "out.txt", "same content"),
		textRound("第二次完成"),
	}}
	a := newApp(t)
	configureWriteRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "p", "m", "白名单")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, err := a.SubmitTask(ctx, sess.ID, "写一次")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approvalID := waitApproval(t, a, sess.ID)
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowSession, "以后别问"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminal(t, a, first.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("第一个任务终态 = %s", done.Status)
	}

	// 把文件改回去，让第二次写入重新成为一次真实变更（参数完全相同）。
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := a.SubmitTask(ctx, sess.ID, "再写一次")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	// 不再调用 ResolveApproval：命中会话白名单应立刻放行。
	if done := waitTerminal(t, a, second.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("第二个任务终态 = %s（%s）", done.Status, done.Error)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "same content" {
		t.Fatalf("第二次落盘内容 = %q", got)
	}

	// 策略放行也必须留痕：第二条 approval.resolved 的 source 是 policy。
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	policies := 0
	for _, ev := range events {
		if ev.Type != domain.EventApprovalResolved {
			continue
		}
		var p domain.ApprovalResolvedPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Source == domain.SourcePolicy {
			policies++
		}
	}
	if policies != 1 {
		t.Fatalf("策略放行应恰好记录一次，实际 %d", policies)
	}
}

func TestConfigureRuntimeExpiresPendingApprovals(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "p", "m", "遗留")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approval := &domain.Approval{
		ID: "apr_ghost", TaskID: task.ID, ToolCallID: "call_g",
		Kind: domain.ApprovalDiff, Risk: domain.RiskMedium, RequestedAt: domain.Now(),
	}
	if err := a.Record(ctx, domain.Transition{TaskID: task.ID, Approval: approval}); err != nil {
		t.Fatalf("写入审批: %v", err)
	}

	// 模拟重启：新的 App 实例在同一份库上启动。
	configureWriteRuntime(t, a, &roundProvider{})

	got, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var expired bool
	for _, ev := range got {
		if ev.Type != domain.EventApprovalResolved {
			continue
		}
		var p domain.ApprovalResolvedPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.ApprovalID == "apr_ghost" && p.Decision == domain.DecisionExpired {
			expired = true
			if p.Source != domain.SourceSystem {
				t.Fatalf("过期来源 = %s，期望 system", p.Source)
			}
		}
	}
	if !expired {
		t.Fatalf("启动时应把遗留审批标成 expired：%v", kindsOf(got))
	}
	stored, err := a.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.TaskCanceled {
		t.Fatalf("幽灵任务终态 = %s，期望 canceled", stored.Status)
	}
}

// 遗留审批对应的工具调用也要收尾：重启后没有任何协程会再推进它，
// 停在 waiting_approval 就是一条永远说不通的记录。
func TestExpirePendingApprovalsCancelsToolCall(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "p", "m", "遗留")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	call := &domain.ToolCall{
		ID:        "call_ghost",
		TaskID:    task.ID,
		SessionID: sess.ID,
		Round:     1,
		Name:      "write_file",
		Args:      []byte(`{"path":"a.txt","content":"x"}`),
		Status:    domain.ToolWaitingApprove,
		CreatedAt: domain.Now(),
	}
	approval := &domain.Approval{
		ID: "apr_ghost", TaskID: task.ID, ToolCallID: call.ID,
		Kind: domain.ApprovalDiff, Risk: domain.RiskMedium, RequestedAt: domain.Now(),
	}
	if err := a.Record(ctx, domain.Transition{
		TaskID: task.ID, ToolCall: call, Approval: approval,
	}); err != nil {
		t.Fatalf("写入待审批: %v", err)
	}

	configureWriteRuntime(t, a, &roundProvider{})

	got, err := a.GetToolCall(ctx, call.ID)
	if err != nil {
		t.Fatalf("GetToolCall: %v", err)
	}
	if got.Status != domain.ToolCanceled {
		t.Fatalf("遗留工具调用状态 = %s，期望 canceled", got.Status)
	}
	if got.EndedAt.IsZero() {
		t.Fatal("遗留工具调用应有结束时间")
	}

	// 重启收尾也要补 tool.result：否则这条会话回放出的历史带着孤立的
	// tool_use，下一次提交会被供应商拒绝。
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.ToolResultPayload
	for _, ev := range events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		if err := ev.Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	if result.ToolCallID != string(call.ID) || result.Status != string(domain.ToolCanceled) || !result.IsError {
		t.Fatalf("遗留调用的 tool.result = %+v，期望 canceled", result)
	}
}

// 启动检查要报出「审批已裁决、却没有 tool.result」的调用：这类历史回放会
// 带着孤立的 tool_use，下一次提交会被供应商拒绝。只报有缺口的那条。
func TestCheckApprovalConsistencyReportsMissingToolResult(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "一致性")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	base := domain.ToolCall{
		TaskID: task.ID, SessionID: sess.ID, Round: 1, Name: "write_file",
		Args: []byte(`{}`), Status: domain.ToolCanceled, CreatedAt: domain.Now(),
	}
	missing := base
	missing.ID = "call_missing"
	finished := base
	finished.ID = "call_finished"

	record := func(call domain.ToolCall, approvalID domain.ApprovalID) {
		t.Helper()
		approval := &domain.Approval{
			ID: approvalID, TaskID: task.ID, ToolCallID: call.ID,
			Kind: domain.ApprovalDiff, Risk: domain.RiskMedium,
			Decision: domain.DecisionAllowOnce, Source: domain.SourceUser,
			RequestedAt: domain.Now(), ResolvedAt: domain.Now(),
		}
		if err := a.Record(ctx, domain.Transition{
			TaskID: task.ID, ToolCall: &call, Approval: approval,
		}); err != nil {
			t.Fatalf("写入审批 %s: %v", approvalID, err)
		}
	}
	record(missing, "apr_missing")
	record(finished, "apr_finished")

	// 只有 finished 有工具结果。
	ev, err := domain.NewEvent(domain.EventToolResult, sess.ID, task.ID, domain.ToolResultPayload{
		Tool: "write_file", ToolCallID: string(finished.ID),
		Status: string(domain.ToolCanceled), IsError: true, Output: "已取消",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Record(ctx, domain.Transition{TaskID: task.ID, Events: []domain.Event{ev}}); err != nil {
		t.Fatalf("写入 tool.result: %v", err)
	}

	gaps, err := a.CheckApprovalConsistency(ctx)
	if err != nil {
		t.Fatalf("CheckApprovalConsistency: %v", err)
	}
	if len(gaps) != 1 || gaps[0].ToolCallID != missing.ID {
		t.Fatalf("不一致记录 = %+v，期望只有 %s", gaps, missing.ID)
	}
}

// 崩溃时正在跑的调用没有审批行可查：启动收尾必须把它们补成 canceled，
// 否则回放出的历史会永久带着没有回填的 tool_use。
func TestStartupReapsUnfinishedToolCalls(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "遗留调用")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	base := domain.ToolCall{
		TaskID: task.ID, SessionID: sess.ID, Round: 1,
		Name: "read_file", Args: []byte(`{}`), CreatedAt: domain.Now(),
	}
	calls := []domain.ToolCall{base, base}
	calls[0].ID, calls[0].Status = "call_prepared", domain.ToolPrepared
	calls[1].ID, calls[1].Status = "call_running", domain.ToolRunning
	for i := range calls {
		if err := a.Record(ctx, domain.Transition{
			TaskID: task.ID, ToolCall: &calls[i],
		}); err != nil {
			t.Fatalf("写入工具调用 %s: %v", calls[i].ID, err)
		}
	}

	configureWriteRuntime(t, a, &roundProvider{})

	for _, want := range calls {
		got, err := a.GetToolCall(ctx, want.ID)
		if err != nil {
			t.Fatalf("GetToolCall %s: %v", want.ID, err)
		}
		if got.Status != domain.ToolCanceled {
			t.Fatalf("%s 状态 = %s，期望 canceled", want.ID, got.Status)
		}
		if got.EndedAt.IsZero() {
			t.Fatalf("%s 缺少结束时间", want.ID)
		}
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]int{}
	for _, ev := range events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		var p domain.ToolResultPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Status != string(domain.ToolCanceled) || !p.IsError {
			t.Fatalf("tool.result = %+v，期望 canceled/is_error", p)
		}
		results[p.ToolCallID]++
	}
	for _, want := range calls {
		if results[string(want.ID)] != 1 {
			t.Fatalf("%s 的 tool.result 条数 = %d，期望 1", want.ID, results[string(want.ID)])
		}
	}

	stored, err := a.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.TaskCanceled {
		t.Fatalf("幽灵任务终态 = %s，期望 canceled", stored.Status)
	}
}

// 连工具都还没调用的任务同样要收尾：崩溃时可能正等着模型输出。
func TestStartupCancelsGhostTaskWithoutApproval(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "幽灵任务")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "跑到一半进程没了")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	configureWriteRuntime(t, a, &roundProvider{})

	stored, err := a.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.TaskCanceled {
		t.Fatalf("幽灵任务终态 = %s，期望 canceled", stored.Status)
	}
}

// 启动收尾必须留下可查的告警：补了哪条调用、收了哪个任务，运维要能在
// 启动日志里直接看到，而不是只能从状态反推。
func TestReapUnfinishedLogsWarnings(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "启动告警")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	call := &domain.ToolCall{
		ID: "call_log", TaskID: task.ID, SessionID: sess.ID, Round: 1,
		Name: "read_file", Args: []byte(`{}`),
		Status: domain.ToolRunning, CreatedAt: domain.Now(),
	}
	if err := a.Record(ctx, domain.Transition{TaskID: task.ID, ToolCall: call}); err != nil {
		t.Fatalf("写入工具调用: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := a.ReapUnfinished(ctx, logger); err != nil {
		t.Fatalf("ReapUnfinished: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"启动收尾未完成的工具调用",
		"tool_call=call_log",
		"启动取消遗留任务",
		"task=" + string(task.ID),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("启动日志缺少 %q：\n%s", want, out)
		}
	}
}

// 审批已经裁决、却没有 tool.result 的调用不能留成悬空 tool_use：历史回放会
// 被供应商直接拒绝，整个会话就不可用了。启动收尾补一条 canceled +「结果未知」，
// 既不假装知道它跑没跑，也让历史结构完整。
func TestStartupBackfillsDecidedCallsWithUnknownResult(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	sess, err := a.CreateSession(ctx, t.TempDir(), "", "", "已裁决缺口")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	call := &domain.ToolCall{
		ID: "call_gap", TaskID: task.ID, SessionID: sess.ID, Round: 1,
		Name: "run_command", Args: []byte(`{}`),
		Status: domain.ToolWaitingApprove, ApprovalID: "apr_gap", CreatedAt: domain.Now(),
	}
	approval := &domain.Approval{
		ID: "apr_gap", TaskID: task.ID, ToolCallID: call.ID,
		Kind: domain.ApprovalCommand, Risk: domain.RiskHigh,
		Decision: domain.DecisionAllowOnce, Source: domain.SourceUser,
		RequestedAt: domain.Now(), ResolvedAt: domain.Now(),
	}
	if err := a.Record(ctx, domain.Transition{
		TaskID: task.ID, ToolCall: call, Approval: approval,
	}); err != nil {
		t.Fatalf("写入已裁决审批: %v", err)
	}

	configureWriteRuntime(t, a, &roundProvider{})

	got, err := a.GetToolCall(ctx, call.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ToolCanceled {
		t.Fatalf("调用状态 = %s，期望补成 canceled", got.Status)
	}
	if got.Error != "执行中进程重启，结果未知" {
		t.Fatalf("调用错误 = %q", got.Error)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.ToolResultPayload
	for _, ev := range events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		if err := ev.Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	if result.ToolCallID != string(call.ID) || result.Status != string(domain.ToolCanceled) {
		t.Fatalf("tool.result = %+v，期望 canceled", result)
	}
	if !strings.Contains(result.Output, "结果未知") {
		t.Fatalf("tool.result 应说明结果未知：%q", result.Output)
	}

	// 补写之后一致性检查不应再报缺口。
	gaps, err := a.CheckApprovalConsistency(ctx)
	if err != nil {
		t.Fatalf("CheckApprovalConsistency: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("补写后仍有缺口：%+v", gaps)
	}
}

func hasEvent(events []domain.Event, kind domain.EventKind) bool {
	for _, ev := range events {
		if ev.Type == kind {
			return true
		}
	}
	return false
}

func kindsOf(events []domain.Event) []domain.EventKind {
	out := make([]domain.EventKind, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}
