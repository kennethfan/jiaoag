package agent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

type fakeCompactor struct {
	calls  int
	text   string
	err    error
	inputs [][]llm.Message
}

func (f *fakeCompactor) Compact(_ context.Context, history []llm.Message) (agent.CompactSummary, error) {
	f.calls++
	f.inputs = append(f.inputs, history)
	if f.err != nil {
		return agent.CompactSummary{}, f.err
	}
	return agent.CompactSummary{
		Text:             f.text,
		Model:            "compact-model",
		PromptTokens:     120,
		CompletionTokens: 30,
	}, nil
}

func readFileRound(id string) streamScript {
	return streamScript{chunks: []llm.Chunk{
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: id, Name: "read_file"}},
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: `{"path":"big.txt"}`}},
		{StopReason: llm.StopToolUse},
	}}
}

func compactFinalRound(text string) streamScript {
	return streamScript{chunks: []llm.Chunk{
		{TextDelta: text},
		{StopReason: llm.StopEnd},
	}}
}

func newCompactAgent(t *testing.T, provider llm.Provider, rec *fakeRecorder, comp agent.Compactor) (*agent.Agent, domain.Task) {
	t.Helper()
	dir := t.TempDir()
	// 单条工具结果足够大，压缩才有意义（低于 minCompactOldChars 会跳过）。
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("x", 4000)), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewReadFile(ws))
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		Provider:          provider,
		Tools:             reg,
		Workspace:         ws,
		Recorder:          rec,
		Model:             "test-model",
		MaxRounds:         8,
		ContextBudget:     600,
		Compact:           true,
		CompactKeepRounds: 2,
		Compactor:         comp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag, domain.Task{ID: "tsk_1", SessionID: "ses_1", Prompt: "读文件并总结", Status: domain.TaskRunning}
}

func TestAgentCompactsContextBeforeRound(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{
		readFileRound("c1"),
		readFileRound("c2"),
		readFileRound("c3"),
		readFileRound("c4"),
		compactFinalRound("读完了"),
	}}
	rec := &fakeRecorder{}
	comp := &fakeCompactor{text: "用户要求读 big.txt；已读 4 次，内容为 x。"}
	ag, task := newCompactAgent(t, provider, rec, comp)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status := rec.lastStatus(); status.To != domain.TaskCompleted {
		t.Fatalf("终态 = %s，期望 completed", status.To)
	}
	if comp.calls != 1 {
		t.Fatalf("压缩调用次数 = %d，期望 1", comp.calls)
	}
	if len(comp.inputs[0]) == 0 {
		t.Fatal("压缩输入为空")
	}

	var compacted int
	for _, ev := range rec.events {
		if ev.Type == domain.EventContextCompacted {
			compacted++
			var p domain.ContextCompactedPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatalf("解码压缩事件: %v", err)
			}
			if p.CoveredMessages == 0 || p.Summary == "" {
				t.Fatalf("压缩事件内容不完整：%+v", p)
			}
		}
	}
	if compacted != 1 {
		t.Fatalf("context.compacted 事件数 = %d，期望 1", compacted)
	}

	audits := recordedAudits(rec)
	var audit domain.AuditRecord
	for _, a := range audits {
		if a.Action == "context.compact" {
			audit = a
		}
	}
	if audit.Action != "context.compact" {
		t.Fatalf("缺少 context.compact 审计：%+v", audits)
	}
	if audit.Actor != "system" {
		t.Fatalf("审计 actor = %q，期望 system", audit.Actor)
	}

	// 压缩后这一轮的请求应以摘要开头。
	if len(provider.requests) < 4 {
		t.Fatalf("请求数 = %d，期望至少 4", len(provider.requests))
	}
	first := provider.requests[3].Messages[0]
	if got := first.Blocks[0].Text; !strings.HasPrefix(got, "[早前对话摘要]") {
		t.Fatalf("压缩后的请求首条不是摘要：%q", got)
	}
}

func TestAgentCompactionFailureKeepsTaskRunning(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{
		readFileRound("c1"),
		readFileRound("c2"),
		readFileRound("c3"),
		readFileRound("c4"),
		compactFinalRound("仍然完成"),
	}}
	rec := &fakeRecorder{}
	comp := &fakeCompactor{err: errors.New("摘要服务超时")}
	ag, task := newCompactAgent(t, provider, rec, comp)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status := rec.lastStatus(); status.To != domain.TaskCompleted {
		t.Fatalf("压缩失败不应影响任务终态：%s", status.To)
	}
	var failed bool
	for _, audit := range recordedAudits(rec) {
		if audit.Action == "context.compact_failed" {
			failed = true
		}
		if audit.Action == "context.compact" {
			t.Fatalf("失败时不应记录成功审计：%+v", audit)
		}
	}
	if !failed {
		t.Fatal("缺少 context.compact_failed 审计")
	}
	for _, ev := range rec.events {
		if ev.Type == domain.EventContextCompacted {
			t.Fatal("失败时不应写入压缩事件")
		}
	}
	if len(provider.requests) < 4 {
		t.Fatalf("请求数 = %d，期望至少 4", len(provider.requests))
	}
	if text := provider.requests[3].Messages[0].Blocks[0].Text; strings.HasPrefix(text, "[早前对话摘要]") {
		t.Fatalf("失败后历史不应被摘要替换：%q", text)
	}
}
