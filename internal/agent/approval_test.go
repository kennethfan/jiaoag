package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// writeRound 是一轮「模型要求写文件」的流式响应。
func writeRound(callID, path, content string) streamScript {
	return streamScript{chunks: []llm.Chunk{
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: callID, Name: "write_file"}},
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: `{"path":"` + path + `","content":"` + content + `"}`}},
		{Done: true, StopReason: llm.StopToolUse},
	}}
}

func finalRound(text string) streamScript {
	return streamScript{chunks: []llm.Chunk{{TextDelta: text}, {Done: true}}}
}

// newWriteAgent 与 newTestAgent 相同，但把工作区目录也返回：写入用例要
// 直接检查磁盘，而不能只信 agent 自己的事件。
func newWriteAgent(t *testing.T, provider llm.Provider, rec *fakeRecorder, maxRounds int) (*agent.Agent, domain.Task, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
		tools.NewWriteFile(ws), tools.NewEditFile(ws))
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		Provider:      provider,
		Tools:         reg,
		Workspace:     ws,
		Recorder:      rec,
		Model:         "test-model",
		MaxRounds:     maxRounds,
		ContextBudget: 60000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	task := domain.Task{
		ID:        "tsk_1",
		SessionID: "ses_1",
		Status:    domain.TaskRunning,
		Prompt:    "改一下文件",
	}
	return ag, task, dir
}

func mustRead(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("读取 %s: %v", rel, err)
	}
	return string(data)
}

func TestApproveWritesFile(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "out.txt", `line one\nline two`),
		finalRound("写好了"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := mustRead(t, dir, "out.txt"); got != "line one\nline two" {
		t.Fatalf("落盘内容 = %q", got)
	}
	if rec.lastStatus().To != domain.TaskCompleted {
		t.Fatalf("终态 = %s", rec.lastStatus().To)
	}

	kinds := strings.Join(rec.kinds(), ",")
	want := []string{
		"tool.requested", "tool.prepared", "proposal.created", "approval.requested",
		"approval.resolved", "proposal.applied", "tool.result",
	}
	prev := -1
	for _, k := range want {
		idx := indexAfter(rec.kinds(), k, prev)
		if idx < 0 {
			t.Fatalf("事件序列缺少 %s（顺序要求）\n实际：%s", k, kinds)
		}
		prev = idx
	}
	if len(rec.approvals) != 1 {
		t.Fatalf("应只发起一次审批，实际 %d", len(rec.approvals))
	}
	if rec.approvals[0].Tool != "write_file" {
		t.Fatalf("审批对象 = %s", rec.approvals[0].Tool)
	}
}

func TestDenyLeavesDiskUntouched(t *testing.T) {
	rec := &fakeRecorder{
		decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
			return agent.ApprovalResult{Decision: domain.DecisionDeny, Source: domain.SourceUser, Note: "别动这个文件"}, nil
		},
	}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "out.txt", "should not exist"),
		finalRound("好的，我不写了"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("拒绝后不该创建文件")
	}
	if rec.lastStatus().To != domain.TaskCompleted {
		t.Fatalf("拒绝不是任务失败，终态 = %s", rec.lastStatus().To)
	}
	if !hasKind(rec.kinds(), "proposal.rejected") {
		t.Fatalf("缺少 proposal.rejected：%v", rec.kinds())
	}

	// 回给模型的必须是工具错误，它才能据此换方案；但 event 里的 status 是
	// denied，不能退化成「工具失败」——那是另一回事。
	res := lastToolResult(t, rec)
	if !res.IsError || res.Status != string(domain.ToolDenied) {
		t.Fatalf("tool.result = %+v，期望 denied", res)
	}

	// 谁拒绝的、为什么拒绝必须留在日志里：审计的 actor 与理由都不能丢。
	var (
		action, actor, detail string
	)
	for _, tr := range rec.transitions {
		for _, a := range tr.Audit {
			if a.Action == "tool.denied" {
				action, actor, detail = a.Action, a.Actor, string(a.Detail)
			}
		}
	}
	if action == "" || actor != "user" {
		t.Fatalf("审计 = %+v，期望 tool.denied/user", action)
	}
	if !strings.Contains(detail, "别动这个文件") {
		t.Fatalf("审计应带用户理由，实际 %s", detail)
	}
	var reason string
	for _, ev := range rec.events {
		if ev.Type != domain.EventProposalRejected {
			continue
		}
		var p domain.ProposalRejectedPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		reason = p.Reason
	}
	if !strings.Contains(reason, "别动这个文件") {
		t.Fatalf("proposal.rejected 应保留用户理由，实际 %q", reason)
	}
}

func TestStaleProposalIsRejected(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "out.txt", "mine"),
		finalRound("放弃"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	// 批准的同时把文件改掉：模拟「审阅期间别人动过」。
	rec.decide = func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("别人写的"), 0o600); err != nil {
			t.Fatalf("伪造并发修改: %v", err)
		}
		return agent.ApprovalResult{Decision: domain.DecisionAllowOnce, Source: domain.SourceUser}, nil
	}

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := mustRead(t, dir, "out.txt"); got != "别人写的" {
		t.Fatalf("CAS 失败时不该覆盖，实际 %q", got)
	}
	if !hasKind(rec.kinds(), "proposal.rejected") {
		t.Fatalf("缺少 proposal.rejected：%v", rec.kinds())
	}
	if !strings.Contains(toolResultText(t, rec), "stale_proposal") {
		t.Fatalf("工具结果应说明 stale_proposal，实际 %q", toolResultText(t, rec))
	}
}

func TestCancelWhileWaitingExpiresApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRecorder{}
	rec.decide = func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		cancel()
		return agent.ApprovalResult{}, context.Canceled
	}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "out.txt", "x"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled", rec.lastStatus().To)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("取消后不该落盘")
	}

	// 审批必须被收尾，否则库里会留一条永远等不到的 pending。
	var resolved bool
	for _, tr := range rec.transitions {
		if tr.Approval == nil {
			continue
		}
		if tr.Approval.Decision == domain.DecisionExpired {
			resolved = true
			if tr.Approval.Source != domain.SourceSystem {
				t.Fatalf("过期来源 = %s，期望 system", tr.Approval.Source)
			}
		}
	}
	if !resolved {
		t.Fatal("取消后应把审批标成 expired")
	}

	// 提案的终态也要进事件日志：只改投影，回放就会看到一条永远 pending 的提案。
	if !hasKind(rec.kinds(), "proposal.rejected") {
		t.Fatalf("取消后应记录 proposal.rejected：%v", rec.kinds())
	}

	// 这次调用没有结果就结束了，必须补一条 canceled 的 tool.result：否则
	// 历史里留下一个没有回填的 tool_use，下一次请求会被供应商拒绝。
	if res := lastToolResult(t, rec); res.Status != string(domain.ToolCanceled) || !res.IsError {
		t.Fatalf("取消后的 tool.result = %+v，期望 canceled", res)
	}

	// 工具调用必须收尾，否则 tool_calls 永远停在 waiting_approval。
	var canceled bool
	for _, tr := range rec.transitions {
		if tr.ToolCall != nil && tr.ToolCall.Status == domain.ToolCanceled {
			canceled = true
		}
	}
	if !canceled {
		t.Fatal("取消后应把工具调用标成 canceled")
	}

	// 取消路径的状态也要自洽：进入审批 → 回到 running → canceled。
	// finish 的 From 硬编码 running，所以审批收尾必须先回 running。
	var entered, resumed bool
	for _, ev := range rec.events {
		if ev.Type != domain.EventTaskStatus {
			continue
		}
		var p domain.TaskStatusPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatalf("解码 task.status: %v", err)
		}
		switch {
		case p.From == domain.TaskRunning && p.To == domain.TaskWaitingApproval:
			entered = true
		case p.From == domain.TaskWaitingApproval && p.To == domain.TaskRunning:
			resumed = true
		}
	}
	if !entered || !resumed {
		t.Fatalf("取消路径的审批状态事件不完整（进入=%v 返回=%v）：%v", entered, resumed, rec.kinds())
	}
}

// 批准与取消撞在同一瞬间：批准是既成事实，必须落成 allow 而不是 expired；
// 但任务已经取消，不能再动磁盘，调用要以 canceled 收尾且只留一条 tool.result。
func TestAllowAtCancelMomentStillResolvesApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		cancel()
		return agent.ApprovalResult{Decision: domain.DecisionAllowOnce, Source: domain.SourceUser}, nil
	}}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "out.txt", "x"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled", rec.lastStatus().To)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("取消后不该落盘")
	}

	// 审批不能因为取消就退回 pending 或被改写成 expired。
	var decision domain.Decision
	for _, tr := range rec.transitions {
		if tr.Approval != nil {
			decision = tr.Approval.Decision
		}
	}
	if decision != domain.DecisionAllowOnce {
		t.Fatalf("审批决议 = %q，期望 allow_once", decision)
	}

	var results int
	for _, ev := range rec.events {
		if ev.Type == domain.EventToolResult {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("tool.result 条数 = %d，期望 1", results)
	}
	if res := lastToolResult(t, rec); res.Status != string(domain.ToolCanceled) || !res.IsError {
		t.Fatalf("tool.result = %+v，期望 canceled", res)
	}
}

// 审批等待是人的时间，不该算进工具耗时：否则用户离开十分钟再批准，
// tool.result 会写一个 600000 ms。
func TestApprovalWaitExcludedFromDuration(t *testing.T) {
	rec := &fakeRecorder{}
	rec.decide = func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		time.Sleep(40 * time.Millisecond)
		return agent.ApprovalResult{Decision: domain.DecisionAllowOnce, Source: domain.SourceUser}, nil
	}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "out.txt", "x"),
		finalRound("完成"),
	}}
	ag, task, _ := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var ms int64 = -1
	for _, ev := range rec.events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		var p domain.ToolResultPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		ms = p.DurationMS
	}
	if ms < 0 {
		t.Fatal("缺少 tool.result")
	}
	if ms >= 40 {
		t.Fatalf("duration 里含审批等待：%d ms", ms)
	}
}

func hasKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

func indexAfter(kinds []string, want string, after int) int {
	for i := after + 1; i < len(kinds); i++ {
		if kinds[i] == want {
			return i
		}
	}
	return -1
}

func toolResultText(t *testing.T, rec *fakeRecorder) string {
	t.Helper()
	out := ""
	for _, ev := range rec.events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		var p domain.ToolResultPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		out += p.Output
	}
	return out
}
