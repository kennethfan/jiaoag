package agent_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

type commandBackend struct {
	result execpkg.Result
	// onRun 在「执行」时触发：用它模拟执行中途取消；err 是这次执行的返回值。
	onRun func()
	err   error

	runs  int
	plans []execpkg.Plan
	specs []execpkg.Spec
}

func (b *commandBackend) Name() string { return "fake" }

func (b *commandBackend) Prepare(_ context.Context, cmd execpkg.Command) (execpkg.Plan, error) {
	return execpkg.Plan{Backend: "fake", Version: "fake/1", Argv: cmd.Argv, Cwd: cmd.Cwd, Env: cmd.Env, Timeout: cmd.Timeout}, nil
}

func (b *commandBackend) Run(_ context.Context, plan execpkg.Plan, spec execpkg.Spec) (execpkg.Result, error) {
	b.runs++
	b.plans = append(b.plans, plan)
	b.specs = append(b.specs, spec)
	if b.onRun != nil {
		b.onRun()
	}
	if b.err != nil {
		return execpkg.Result{}, b.err
	}
	return b.result, nil
}

func commandRound(callID string, argv []string) streamScript {
	args, _ := json.Marshal(map[string]any{"argv": argv})
	return streamScript{chunks: []llm.Chunk{
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: callID, Name: "run_command"}},
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: string(args)}},
		{Done: true, StopReason: llm.StopToolUse},
	}}
}

func newCommandAgent(t *testing.T, provider llm.Provider, rec *fakeRecorder, b execpkg.Backend) (*agent.Agent, domain.Task, string) {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewRunCommand(ws, tools.RuntimeEnv{
		Backend: b, OutputDir: dir, Timeout: time.Second, EventLimit: 8 << 10, LogLimit: 1 << 20,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		Provider:      provider,
		Tools:         reg,
		Workspace:     ws,
		Recorder:      rec,
		Model:         "test-model",
		MaxRounds:     4,
		ContextBudget: 20000,
		OutputDir:     dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "tsk_1", SessionID: "ses_1", Status: domain.TaskRunning, Prompt: "跑一下命令"}
	return ag, task, dir
}

func recordedAudits(rec *fakeRecorder) []domain.AuditRecord {
	var out []domain.AuditRecord
	for _, tr := range rec.transitions {
		out = append(out, tr.Audit...)
	}
	return out
}

func lastToolResult(t *testing.T, rec *fakeRecorder) domain.ToolResultPayload {
	t.Helper()
	for i := len(rec.events) - 1; i >= 0; i-- {
		if rec.events[i].Type != domain.EventToolResult {
			continue
		}
		var p domain.ToolResultPayload
		if err := rec.events[i].Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Fatal("缺少 tool.result")
	return domain.ToolResultPayload{}
}

// tool_call_id 来自供应商响应，模型可以构造它；它不是路径片段。恶意 id
// 不能把全文日志写到任务输出目录之外（capture 会 MkdirAll + O_TRUNC）。
func TestMaliciousToolCallIDCannotEscapeOutputDir(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("../../../../escape", []string{"cat", "a.txt"}),
		finalRound("跑完了"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 0}}
	ag, task, dir := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(b.specs) != 1 || b.specs[0].LogPath == "" {
		t.Fatalf("命令应执行并带日志路径：%+v", b.specs)
	}
	logPath := b.specs[0].LogPath
	rel, err := filepath.Rel(dir, logPath)
	if err != nil {
		t.Fatalf("计算日志相对路径: %v", err)
	}
	wantPrefix := filepath.Join("ses_1", "tsk_1") + string(filepath.Separator)
	if !strings.HasPrefix(rel, wantPrefix) {
		t.Fatalf("日志路径逃出任务输出目录：%q（相对 %q）", logPath, rel)
	}
}

func TestWhitelistedCommandRunsWithoutApproval(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"cat", "a.txt"}),
		finalRound("跑完了"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 0, Output: "hello\n", OutputPath: "/tmp/call.log"}}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) != 0 {
		t.Fatalf("白名单命令不应打断用户，实际发起 %d 次审批", len(rec.approvals))
	}
	if b.runs != 1 {
		t.Fatalf("命令应执行一次，实际 %d", b.runs)
	}
	if b.specs[0].LogPath == "" {
		t.Fatal("命令全文日志路径应写入 Prepared.LogPath")
	}
	audits := recordedAudits(rec)
	if len(audits) != 1 || audits[0].Action != "exec.allow" || audits[0].Actor != "policy" {
		t.Fatalf("审计 = %+v", audits)
	}
	var detail map[string]any
	if err := json.Unmarshal(audits[0].Detail, &detail); err != nil {
		t.Fatalf("审计 detail: %v", err)
	}
	if detail["backend_version"] != "fake/1" {
		t.Fatalf("审计 detail 缺后端版本：%s", audits[0].Detail)
	}
	// 准备阶段不能投影成 running（会让人以为进程已经起了），但真正执行前必须
	// 显式写一次 running：否则一次跑几分钟的 go test 在历史里永远显示「待审批」。
	running := 0
	for _, tr := range rec.transitions {
		if tr.ToolCall != nil && tr.ToolCall.Status == domain.ToolRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running 投影次数 = %d，期望恰 1 次", running)
	}
	payload := lastToolResult(t, rec)
	if payload.Output != "hello\n" || payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("tool.result = %+v", payload)
	}
	if payload.OutputPath != "/tmp/call.log" {
		t.Fatalf("tool.result 应带 output_path，实际 %q", payload.OutputPath)
	}
	if rec.lastStatus().To != domain.TaskCompleted {
		t.Fatalf("终态 = %s", rec.lastStatus().To)
	}
}

func TestNonWhitelistedCommandWaitsForApproval(t *testing.T) {
	// 在裁决那一刻看一眼投影：命令还停在 waiting_approval，而不是 running。
	var statusAtDecision domain.ToolCallStatus
	rec := &fakeRecorder{}
	rec.decide = func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		for _, tr := range rec.transitions {
			if tr.ToolCall != nil {
				statusAtDecision = tr.ToolCall.Status
			}
		}
		return agent.ApprovalResult{Decision: domain.DecisionAllowOnce, Source: domain.SourceUser}, nil
	}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"rm", "-rf", "build"}),
		finalRound("删了"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 0}}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) != 1 {
		t.Fatalf("应发起一次审批，实际 %d", len(rec.approvals))
	}
	if b.runs != 1 {
		t.Fatalf("批准后应执行，实际 %d", b.runs)
	}
	if statusAtDecision != domain.ToolWaitingApprove {
		t.Fatalf("裁决前的工具状态 = %q，期望 waiting_approval", statusAtDecision)
	}
	// 裁决之后进程即将启动：必须补一次 running，把「等审批」与「正在跑」分开。
	running := 0
	for _, tr := range rec.transitions {
		if tr.ToolCall != nil && tr.ToolCall.Status == domain.ToolRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("批准后的 running 投影 = %d，期望恰 1 次", running)
	}
	for _, ev := range rec.events {
		if ev.Type == domain.EventApprovalRequested {
			var p domain.ApprovalRequestedPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Kind != domain.ApprovalCommand || len(p.Command) != 3 || p.Cwd == "" || p.Backend != "fake" {
				t.Fatalf("审批载荷缺少 argv/cwd/backend：%+v", p)
			}
		}
	}
	audits := recordedAudits(rec)
	if len(audits) != 1 || audits[0].Action != "exec.approved" || audits[0].Actor != "user" {
		t.Fatalf("审计 = %+v", audits)
	}
}

func TestDeniedCommandNeverRuns(t *testing.T) {
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		return agent.ApprovalResult{Decision: domain.DecisionDeny, Source: domain.SourceUser, Note: "别删"}, nil
	}}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"rm", "-rf", "build"}),
		finalRound("好，不删"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 0}}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b.runs != 0 {
		t.Fatalf("拒绝后不应启动进程，实际执行 %d 次", b.runs)
	}
	audits := recordedAudits(rec)
	if len(audits) != 1 || audits[0].Action != "exec.denied" || audits[0].Actor != "user" {
		t.Fatalf("审计 = %+v", audits)
	}
	res := lastToolResult(t, rec)
	if !res.IsError || res.Status != string(domain.ToolDenied) {
		t.Fatalf("tool.result = %+v，期望 denied", res)
	}
	if !strings.Contains(res.Output, "别删") {
		t.Fatalf("拒绝理由应回给模型，实际 %q", res.Output)
	}
}

// allow_session 命中会话白名单时，放行是一次策略决定：审计的 actor 必须是
// policy，而不是又一条「用户批准」。
func TestPolicyApprovedCommandAuditedAsPolicy(t *testing.T) {
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		return agent.ApprovalResult{Decision: domain.DecisionAllowSession, Source: domain.SourcePolicy}, nil
	}}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"rm", "-rf", "build"}),
		finalRound("删了"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 0}}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b.runs != 1 {
		t.Fatalf("批准后应执行，实际 %d", b.runs)
	}
	audits := recordedAudits(rec)
	if len(audits) != 1 || audits[0].Action != "exec.approved" || audits[0].Actor != "policy" {
		t.Fatalf("审计 = %+v，期望 exec.approved/policy", audits)
	}
	if res := lastToolResult(t, rec); res.Status != string(domain.ToolSucceeded) {
		t.Fatalf("tool.result = %+v，期望 succeeded", res)
	}
}

func TestCommandTimeoutIsAudited(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"cat", "a.txt"}),
		finalRound("超时了"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 124, TimedOut: true, Output: "partial"}}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	audits := recordedAudits(rec)
	if len(audits) != 1 || audits[0].Action != "exec.timeout" || audits[0].Actor != "system" {
		t.Fatalf("审计 = %+v", audits)
	}
	if !lastToolResult(t, rec).IsError {
		t.Fatal("超时应是工具错误")
	}
}

func TestMetacharCommandNeedsApproval(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"go", "test", "./...", "|", "tee"}),
		finalRound("跑了"),
	}}
	b := &commandBackend{result: execpkg.Result{ExitCode: 0}}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) != 1 {
		t.Fatalf("含元字符的命令必须审批，实际 %d 次", len(rec.approvals))
	}
}

func TestCancelWhileWaitingExpiresCommandApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		cancel()
		return agent.ApprovalResult{}, context.Canceled
	}}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"rm", "-rf", "build"}),
	}}
	b := &commandBackend{}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b.runs != 0 {
		t.Fatal("取消后不应执行")
	}
	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s", rec.lastStatus().To)
	}
	var expired, canceled bool
	for _, tr := range rec.transitions {
		if tr.Approval != nil && tr.Approval.Decision == domain.DecisionExpired {
			expired = true
		}
		if tr.ToolCall != nil && tr.ToolCall.Status == domain.ToolCanceled {
			canceled = true
		}
	}
	if !expired || !canceled {
		t.Fatalf("审批与工具调用都应收尾：expired=%v canceled=%v", expired, canceled)
	}
	if res := lastToolResult(t, rec); res.Status != string(domain.ToolCanceled) || !res.IsError {
		t.Fatalf("取消后的 tool.result = %+v，期望 canceled", res)
	}
}

// 批准与取消撞在同一瞬间：审批要落成 allow，但进程不能启动；
// 调用以 canceled 收尾并留下 tool.result。
func TestCommandAllowAtCancelMomentStillResolvesApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		cancel()
		return agent.ApprovalResult{Decision: domain.DecisionAllowOnce, Source: domain.SourceUser}, nil
	}}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"rm", "-rf", "build"}),
	}}
	b := &commandBackend{}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled", rec.lastStatus().To)
	}
	if b.runs != 0 {
		t.Fatalf("取消后不该启动进程，runs=%d", b.runs)
	}

	var decision domain.Decision
	for _, tr := range rec.transitions {
		if tr.Approval != nil {
			decision = tr.Approval.Decision
		}
	}
	if decision != domain.DecisionAllowOnce {
		t.Fatalf("审批决议 = %q，期望 allow_once", decision)
	}
	if res := lastToolResult(t, rec); res.Status != string(domain.ToolCanceled) || !res.IsError {
		t.Fatalf("tool.result = %+v，期望 canceled", res)
	}
}

// 取消发生在命令执行中：进程被杀之后，调用仍要以 canceled 收尾并留下
// tool.result，否则历史里会留下一个没有回填的 tool_use。
func TestCancelDuringCommandExecutionIsRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		commandRound("call_c", []string{"go", "test", "./..."}),
	}}
	b := &commandBackend{err: context.Canceled, onRun: cancel}
	ag, task, _ := newCommandAgent(t, provider, rec, b)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled", rec.lastStatus().To)
	}
	res := lastToolResult(t, rec)
	if res.Status != string(domain.ToolCanceled) || !res.IsError {
		t.Fatalf("tool.result = %+v，期望 canceled", res)
	}
	var canceled bool
	for _, tr := range rec.transitions {
		if tr.ToolCall != nil && tr.ToolCall.Status == domain.ToolCanceled {
			canceled = true
		}
	}
	if !canceled {
		t.Fatal("取消后工具调用应标成 canceled")
	}
	audits := recordedAudits(rec)
	if len(audits) != 1 || audits[0].Action != "exec.failed" || audits[0].Actor != "system" {
		t.Fatalf("审计 = %+v，期望 exec.failed/system", audits)
	}
}
