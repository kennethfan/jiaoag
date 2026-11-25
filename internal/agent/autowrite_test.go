package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// newAutoWriteAgent 与 newWriteAgent 同形，但可以指定写入审批模式。
func newAutoWriteAgent(t *testing.T, provider llm.Provider, rec *fakeRecorder, mode string) (*agent.Agent, domain.Task, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewReadFile(ws), tools.NewWriteFile(ws), tools.NewEditFile(ws))
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
		ContextBudget: 60000,
		WriteApproval: mode,
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

func TestWorkspaceWriteSkipsApproval(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_1", "b.txt", "auto-write"),
		finalRound("done"),
	}}
	ag, task, dir := newAutoWriteAgent(t, provider, rec, agent.WriteApprovalWorkspace)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) != 0 {
		t.Fatalf("工作区内写入不该请求审批，实际 %d 次", len(rec.approvals))
	}
	data, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil {
		t.Fatalf("文件未写入: %v", err)
	}
	if string(data) != "auto-write" {
		t.Fatalf("内容不对: %q", data)
	}

	var applied domain.ProposalAppliedPayload
	var kinds []string
	for _, ev := range rec.events {
		kinds = append(kinds, string(ev.Type))
		if ev.Type != domain.EventProposalApplied {
			continue
		}
		if err := ev.Decode(&applied); err != nil {
			t.Fatalf("解析 proposal.applied: %v", err)
		}
	}
	if !hasKind(kinds, string(domain.EventProposalCreated)) {
		t.Fatal("免审写入也要落 proposal.created，否则事后看不到改了什么")
	}
	if !applied.Auto || applied.Mode != "workspace" {
		t.Fatalf("proposal.applied 应标记 auto/workspace，实际 %+v", applied)
	}

	audits := recordedAudits(rec)
	found := false
	for _, a := range audits {
		if a.Action != "write.auto" {
			continue
		}
		found = true
		if a.Actor != string(domain.SourcePolicy) {
			t.Fatalf("免审写入的审计 actor 应为 policy，实际 %q", a.Actor)
		}
		if !strings.Contains(string(a.Detail), `"path":"b.txt"`) || !strings.Contains(string(a.Detail), `"auto":true`) {
			t.Fatalf("审计 detail 不完整: %s", a.Detail)
		}
	}
	if !found {
		t.Fatal("缺少 write.auto 审计")
	}
}

func TestSensitiveWorkspacePathStillAsks(t *testing.T) {
	for _, path := range []string{".env", ".npmrc", "AGENTS.md", "id_rsa"} {
		t.Run(path, func(t *testing.T) {
			rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
				return agent.ApprovalResult{Decision: domain.DecisionDeny, Source: domain.SourceUser}, nil
			}}
			provider := &scriptedProvider{scripts: []streamScript{
				writeRound("call_1", path, "should-not-land"),
				finalRound("done"),
			}}
			ag, task, dir := newAutoWriteAgent(t, provider, rec, agent.WriteApprovalWorkspace)

			if err := ag.Run(context.Background(), task); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(rec.approvals) == 0 {
				t.Fatalf("%s 属于敏感路径，应该请求审批", path)
			}
			if _, err := os.Stat(filepath.Join(dir, path)); err == nil {
				t.Fatalf("%s 未批准却落盘了", path)
			}
		})
	}
}

func TestWriteApprovalAlwaysAsks(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_1", "b.txt", "always"),
		finalRound("done"),
	}}
	// 空串是库的零值，必须落在最安全的一档。
	ag, task, _ := newAutoWriteAgent(t, provider, rec, "")

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) == 0 {
		t.Fatal("零值配置应保持每次审批")
	}
}

func TestWriteApprovalOffNeverAsks(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_1", ".env", "off"),
		finalRound("done"),
	}}
	ag, task, dir := newAutoWriteAgent(t, provider, rec, agent.WriteApprovalOff)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.approvals) != 0 {
		t.Fatalf("off 模式不该请求审批，实际 %d 次", len(rec.approvals))
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Fatalf("off 模式应直接写入: %v", err)
	}
}

// 大内容写入不该被审批拦下，但也要能完成：验证估算与落盘链路。
func TestWorkspaceWriteLargeContent(t *testing.T) {
	rec := &fakeRecorder{}
	var content string
	for i := 0; i < 200; i++ {
		content += "line " + strconv.Itoa(i) + "\\n"
	}
	provider := &scriptedProvider{scripts: []streamScript{
		{chunks: []llm.Chunk{
			{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}},
			{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: `{"path":"big.txt","content":` + strconv.Quote(content) + `}`}},
			{Done: true, StopReason: llm.StopToolUse},
		}},
		finalRound("done"),
	}}
	ag, task, dir := newAutoWriteAgent(t, provider, rec, agent.WriteApprovalWorkspace)
	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "big.txt"))
	if err != nil {
		t.Fatalf("文件未写入: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("写入内容为空")
	}
}

// 工作区里放一个指向外部的软链，不能当成「工作区内」放行。
func TestSymlinkEscapeStillAsks(t *testing.T) {
	// 不能用 t.TempDir()：它位于 TMPDIR 下，而临时目录本来就允许免审写入，
	// 这样测不出「写到工作区外」。放在仓库目录里的临时目录（用完删掉）。
	base, err := os.MkdirTemp(".", ".autowrite-escape-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	wsDir := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{wsDir, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(wsDir, "escape")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	ws, err := workspace.New(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewReadFile(ws), tools.NewWriteFile(ws))
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{decide: func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		return agent.ApprovalResult{Decision: domain.DecisionDeny, Source: domain.SourceUser}, nil
	}}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_1", "escape/x.txt", "escaped"),
		finalRound("done"),
	}}
	ag, err := agent.New(agent.Options{
		Provider: provider, Tools: reg, Workspace: ws, Recorder: rec,
		Model: "test-model", MaxRounds: 4, ContextBudget: 60000,
		WriteApproval: agent.WriteApprovalWorkspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ag.Run(context.Background(), domain.Task{
		ID: "tsk_1", SessionID: "ses_1", Status: domain.TaskRunning, Prompt: "写文件",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("经软链写到了工作区外")
	}
	// 两道防线都算通过：工具层直接报「解析后落在工作区外」，或走到审批被拒。
	res := lastToolResult(t, rec)
	if !res.IsError && len(rec.approvals) == 0 {
		t.Fatalf("越界写入既没被工具层拒绝，也没请求审批: %+v", res)
	}
}
