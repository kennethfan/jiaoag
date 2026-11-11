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

type streamScript struct {
	chunks []llm.Chunk
	err    error
	before func(ctx context.Context)
}

type scriptedProvider struct {
	scripts  []streamScript
	requests []llm.Request
	calls    int
}

func (p *scriptedProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (p *scriptedProvider) ChatStream(ctx context.Context, req llm.Request, fn func(llm.Chunk) error) error {
	p.requests = append(p.requests, req)
	index := p.calls
	p.calls++
	script := streamScript{}
	if index < len(p.scripts) {
		script = p.scripts[index]
	}
	if script.before != nil {
		script.before(ctx)
	}
	for _, chunk := range script.chunks {
		if err := fn(chunk); err != nil {
			return err
		}
	}
	if script.err != nil {
		return script.err
	}
	return ctx.Err()
}

type fakeRecorder struct {
	events      []domain.Event
	deltas      []domain.Event
	transitions []domain.Transition
	seq         int64
}

func (r *fakeRecorder) Record(_ context.Context, tr domain.Transition) error {
	r.transitions = append(r.transitions, tr)
	for _, ev := range tr.Events {
		r.seq++
		ev.Seq = domain.Seq(r.seq)
		ev.At = domain.Now()
		r.events = append(r.events, ev)
	}
	return nil
}

func (r *fakeRecorder) Events(context.Context, domain.SessionID) ([]domain.Event, error) {
	return r.events, nil
}

func (r *fakeRecorder) Delta(_ context.Context, ev domain.Event) error {
	r.deltas = append(r.deltas, ev)
	return nil
}

func (r *fakeRecorder) kinds() []string {
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, string(ev.Type))
	}
	return out
}

func (r *fakeRecorder) lastStatus() domain.TaskStatusPayload {
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Type != domain.EventTaskStatus {
			continue
		}
		var p domain.TaskStatusPayload
		_ = r.events[i].Decode(&p)
		return p
	}
	return domain.TaskStatusPayload{}
}

func (r *fakeRecorder) counts() map[string]int {
	out := make(map[string]int)
	for _, kind := range r.kinds() {
		out[kind]++
	}
	return out
}

func newTestAgent(t *testing.T, provider llm.Provider, rec *fakeRecorder, maxRounds int) (*agent.Agent, domain.Task) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws))
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
		Prompt:    "读一下 a.txt",
	}
	return ag, task
}

func TestRunCompletesPlainAnswer(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{{
		chunks: []llm.Chunk{
			{TextDelta: "你好"},
			{TextDelta: "，世界"},
			{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 11, CompletionTokens: 5}},
		},
	}}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 3)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	status := rec.lastStatus()
	if status.To != domain.TaskCompleted {
		t.Fatalf("终态 = %s，期望 completed（事件 %v）", status.To, rec.kinds())
	}
	if status.PromptTok != 11 || status.OutputTok != 5 {
		t.Fatalf("token 未累加：%+v", status)
	}
	last := rec.transitions[len(rec.transitions)-1]
	if last.Usage == nil || last.Usage.PromptTokens != 11 {
		t.Fatalf("迁移未携带 usage：%+v", last.Usage)
	}
	if len(rec.deltas) != 2 {
		t.Fatalf("增量帧数 = %d，期望 2", len(rec.deltas))
	}
	for _, ev := range rec.deltas {
		if ev.Seq != 0 {
			t.Fatalf("增量帧不应有 seq：%d", ev.Seq)
		}
		if ev.Type != domain.EventAssistantDelta {
			t.Fatalf("增量帧类型 = %s", ev.Type)
		}
	}
	if provider.calls != 1 {
		t.Fatalf("模型调用次数 = %d，期望 1", provider.calls)
	}
}

func TestRunExecutesToolThenAnswers(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{
		{chunks: []llm.Chunk{
			{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "read_file"}},
			{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: `{"path":"a.txt"}`}},
			{Done: true, StopReason: llm.StopToolUse},
		}},
		{chunks: []llm.Chunk{
			{TextDelta: "文件内容是 hello world"},
			{Done: true, StopReason: llm.StopEnd},
		}},
	}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 3)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastStatus().To != domain.TaskCompleted {
		t.Fatalf("终态 = %s（事件 %v）", rec.lastStatus().To, rec.kinds())
	}

	counts := rec.counts()
	if counts[string(domain.EventToolRequested)] != 1 || counts[string(domain.EventToolResult)] != 1 {
		t.Fatalf("工具事件数不对：%v", counts)
	}
	var result domain.ToolResultPayload
	for _, ev := range rec.events {
		if ev.Type == domain.EventToolResult {
			if err := ev.Decode(&result); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !strings.Contains(result.Output, "hello world") || result.IsError {
		t.Fatalf("工具结果不对：%+v", result)
	}

	// 第二轮请求必须带上 tool_result，否则模型看不到工具输出。
	if len(provider.requests) != 2 {
		t.Fatalf("请求数 = %d", len(provider.requests))
	}
	found := false
	for _, block := range provider.requests[1].Messages[len(provider.requests[1].Messages)-1].Blocks {
		if block.Kind == llm.BlockToolResult && strings.Contains(block.Result, "hello world") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮请求缺少 tool_result：%+v", provider.requests[1].Messages)
	}
}

func TestRunStopsAtMaxRounds(t *testing.T) {
	toolRound := streamScript{chunks: []llm.Chunk{
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: "call_x", Name: "glob"}},
		{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: `{"pattern":"*.txt"}`}},
		{Done: true, StopReason: llm.StopToolUse},
	}}
	provider := &scriptedProvider{scripts: []streamScript{toolRound, toolRound, toolRound}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 2)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	status := rec.lastStatus()
	if status.To != domain.TaskFailed || status.Reason != "max_rounds" {
		t.Fatalf("期望 max_rounds 失败，得到 %+v", status)
	}
	if provider.calls != 2 {
		t.Fatalf("调用次数 = %d，期望 2", provider.calls)
	}
}

func TestRunCanceledRecordsTerminalState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &scriptedProvider{scripts: []streamScript{{
		before: func(context.Context) { cancel() },
	}}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 3)

	if err := ag.Run(ctx, task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastStatus().To != domain.TaskCanceled {
		t.Fatalf("终态 = %s，期望 canceled（事件 %v）", rec.lastStatus().To, rec.kinds())
	}
}

func TestRunRetriesOnceOnContextOverflow(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{
		{err: llm.ErrContextOverflow},
		{chunks: []llm.Chunk{{TextDelta: "好的"}, {Done: true}}},
	}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 3)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.lastStatus().To != domain.TaskCompleted {
		t.Fatalf("收缩后应完成，得到 %s", rec.lastStatus().To)
	}
	if provider.calls != 2 {
		t.Fatalf("调用次数 = %d，期望 2", provider.calls)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("请求数 = %d", len(provider.requests))
	}
	first := countMessages(provider.requests[0])
	second := countMessages(provider.requests[1])
	if second > first {
		t.Fatalf("收缩后上下文没有变小：%d → %d", first, second)
	}
}

func TestRunFailsAfterSecondOverflow(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{
		{err: llm.ErrContextOverflow},
		{err: llm.ErrContextOverflow},
		{chunks: []llm.Chunk{{TextDelta: "不应到达"}, {Done: true}}},
	}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 3)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	status := rec.lastStatus()
	if status.To != domain.TaskFailed || status.Reason != "context_overflow" {
		t.Fatalf("期望 context_overflow 失败，得到 %+v", status)
	}
	if provider.calls != 2 {
		t.Fatalf("只应重试一次，调用次数 = %d", provider.calls)
	}
}

func TestRunAuthErrorDoesNotRetry(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{{err: llm.ErrAuth}}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 3)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	status := rec.lastStatus()
	if status.To != domain.TaskFailed || status.Reason != "provider_auth" {
		t.Fatalf("期望 provider_auth 失败，得到 %+v", status)
	}
	if provider.calls != 1 {
		t.Fatalf("鉴权失败不应重试，调用次数 = %d", provider.calls)
	}
}

func TestRunFeedsExistingHistoryIntoRequest(t *testing.T) {
	provider := &scriptedProvider{scripts: []streamScript{
		{chunks: []llm.Chunk{{TextDelta: "回答"}, {Done: true}}},
	}}
	rec := &fakeRecorder{}
	ag, task := newTestAgent(t, provider, rec, 2)

	queued, err := domain.NewEvent(domain.EventTaskQueued, task.SessionID, "tsk_0", domain.TaskQueuedPayload{Prompt: "上一轮的问题"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := domain.NewEvent(domain.EventAssistantMessage, task.SessionID, "tsk_0", domain.AssistantMessagePayload{Round: 1, Text: "上一轮的回答"})
	if err != nil {
		t.Fatal(err)
	}
	rec.events = append(rec.events, queued, answer)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	messages := provider.requests[0].Messages
	if len(messages) != 2 {
		t.Fatalf("历史消息数 = %d，期望 2：%+v", len(messages), messages)
	}
	if messages[0].Blocks[0].Text != "上一轮的问题" || messages[1].Blocks[0].Text != "上一轮的回答" {
		t.Fatalf("历史顺序不对：%+v", messages)
	}
}

func countMessages(req llm.Request) int {
	total := 0
	for _, msg := range req.Messages {
		for _, block := range msg.Blocks {
			total += len(block.Text) + len(block.Result) + len(block.Args)
		}
	}
	return total
}
