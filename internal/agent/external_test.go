package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// fakeExternal 是一个最小外部工具：宿主看不到它的实现，只能审批与审计。
type fakeExternal struct {
	trusted  bool
	executed int
	result   tools.Result
}

func (f *fakeExternal) Spec() tools.Spec {
	return tools.Spec{
		Name:        "mcp__fs__read",
		Description: "read a file through MCP",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Effects:     []tools.Effect{tools.EffectProcess},
		Version:     "mcp:fs@1.0.0#abcd1234",
	}
}

func (f *fakeExternal) Prepare(_ context.Context, call tools.Call) (tools.Prepared, error) {
	return tools.Prepared{
		Call: call,
		Spec: f.Spec(),
		Args: call.Args,
		External: &tools.External{
			Source:  "mcp:fs",
			Trusted: f.trusted,
		},
	}, nil
}

func (f *fakeExternal) Execute(context.Context, tools.Prepared) (tools.Result, error) {
	f.executed++
	return f.result, nil
}

func externalRound(callID, args string) streamScript {
	return streamScript{chunks: []llm.Chunk{
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: callID, Name: "mcp__fs__read"}},
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: args}},
		{Done: true, StopReason: llm.StopToolUse},
	}}
}

func newExternalAgent(t *testing.T, provider llm.Provider, rec *fakeRecorder, ext *fakeExternal) (*agent.Agent, domain.Task) {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(ext)
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
	task := domain.Task{ID: "tsk_1", SessionID: "ses_1", Status: domain.TaskRunning, Prompt: "读一下"}
	return ag, task
}

func externalAudits(rec *fakeRecorder) []domain.AuditRecord {
	var out []domain.AuditRecord
	for _, tr := range recordedAudits(rec) {
		if strings.HasPrefix(tr.Action, "external.") {
			out = append(out, tr)
		}
	}
	return out
}

func TestExternalToolWaitsForApproval(t *testing.T) {
	rec := &fakeRecorder{}
	ext := &fakeExternal{result: tools.Result{Content: "file body"}}
	provider := &scriptedProvider{scripts: []streamScript{
		externalRound("call_e", `{"path":"a.txt"}`),
		finalRound("读完了"),
	}}
	ag, task := newExternalAgent(t, provider, rec, ext)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ext.executed != 1 {
		t.Fatalf("批准后应执行一次，实际 %d", ext.executed)
	}
	if len(rec.approvals) != 1 {
		t.Fatalf("审批请求 = %d，期望 1", len(rec.approvals))
	}
	// 审批事件必须带来源与参数：否则面板与事后审计都无从判断调了谁。
	var found bool
	for _, ev := range rec.events {
		if ev.Type != domain.EventApprovalRequested {
			continue
		}
		var p domain.ApprovalRequestedPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Kind != domain.ApprovalExternal {
			continue
		}
		found = true
		if p.Tool != "mcp__fs__read" || p.Source != "mcp:fs" || !strings.Contains(p.Args, "a.txt") {
			t.Fatalf("审批事件缺少来源信息：%+v", p)
		}
	}
	if !found {
		t.Fatalf("缺少 external 审批事件：%v", rec.kinds())
	}
	audits := externalAudits(rec)
	if len(audits) != 1 || audits[0].Action != "external.approved" || audits[0].Actor != "user" {
		t.Fatalf("审计 = %+v，期望一条 external.approved(user)", audits)
	}
}

func TestExternalToolDeniedNeverExecutes(t *testing.T) {
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		return agent.ApprovalResult{Decision: domain.DecisionDeny, Source: domain.SourceUser}, nil
	}}
	ext := &fakeExternal{result: tools.Result{Content: "should not run"}}
	provider := &scriptedProvider{scripts: []streamScript{
		externalRound("call_e", `{"path":"a.txt"}`),
		finalRound("算了"),
	}}
	ag, task := newExternalAgent(t, provider, rec, ext)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ext.executed != 0 {
		t.Fatal("拒绝后不得调用外部工具")
	}
	res := lastToolResult(t, rec)
	if res.Status != string(domain.ToolDenied) || !res.IsError {
		t.Fatalf("tool.result = %+v，期望 denied", res)
	}
	audits := externalAudits(rec)
	if len(audits) != 1 || audits[0].Action != "external.denied" {
		t.Fatalf("审计 = %+v", audits)
	}
}

func TestTrustedExternalSkipsApprovalWithPolicyAudit(t *testing.T) {
	rec := &fakeRecorder{}
	ext := &fakeExternal{trusted: true, result: tools.Result{Content: "ok"}}
	provider := &scriptedProvider{scripts: []streamScript{
		externalRound("call_e", `{"path":"a.txt"}`),
		finalRound("好了"),
	}}
	ag, task := newExternalAgent(t, provider, rec, ext)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) != 0 {
		t.Fatalf("trust=allow 不应弹审批，实际 %d", len(rec.approvals))
	}
	if ext.executed != 1 {
		t.Fatalf("应执行一次，实际 %d", ext.executed)
	}
	audits := externalAudits(rec)
	if len(audits) != 1 || audits[0].Action != "external.allow" || audits[0].Actor != "policy" {
		t.Fatalf("审计 = %+v，期望 external.allow(policy)", audits)
	}
}

func TestCancelWhileWaitingExternalExpiresApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		cancel()
		return agent.ApprovalResult{}, context.Canceled
	}}
	ext := &fakeExternal{result: tools.Result{Content: "nope"}}
	provider := &scriptedProvider{scripts: []streamScript{
		externalRound("call_e", `{"path":"a.txt"}`),
	}}
	ag, task := newExternalAgent(t, provider, rec, ext)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ext.executed != 0 {
		t.Fatal("取消后不得调用外部工具")
	}
	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s", rec.lastStatus().To)
	}
	var expired bool
	for _, tr := range rec.transitions {
		if tr.Approval != nil && tr.Approval.Kind == domain.ApprovalExternal && tr.Approval.Decision == domain.DecisionExpired {
			expired = true
		}
	}
	if !expired {
		t.Fatal("取消后应把外部审批标成 expired")
	}
	if res := lastToolResult(t, rec); res.Status != string(domain.ToolCanceled) {
		t.Fatalf("tool.result = %+v，期望 canceled", res)
	}
}
