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
			return agent.ApprovalResult{Decision: domain.DecisionDeny, Source: domain.SourceUser}, nil
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

	// 回给模型的必须是工具错误，它才能据此换方案。
	last := rec.transitions[len(rec.transitions)-1]
	_ = last
	var found bool
	for _, ev := range rec.events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		var p domain.ToolResultPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.IsError {
			found = true
		}
	}
	if !found {
		t.Fatal("拒绝应以工具错误回给模型")
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
