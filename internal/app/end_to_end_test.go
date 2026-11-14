package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// fakeOpenAI 按轮次返回预置的流式响应：glob → read_file → 纯文本。
func fakeOpenAI(t *testing.T, rounds *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("读取请求体: %v", err)
		}
		round := atomic.AddInt32(rounds, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := sseFrames(round)
		for _, frame := range frames {
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

func sseFrames(round int32) []string {
	data := func(body string) string { return "data: " + body + "\n\n" }
	usage := data(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)

	switch round {
	case 1:
		return []string{
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_glob","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"**/*.go\"}"}}]},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
			usage,
			data(`[DONE]`),
		}
	case 2:
		return []string{
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
			usage,
			data(`[DONE]`),
		}
	default:
		return []string{
			data(`{"choices":[{"delta":{"content":"仓库根目录有"},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{"content":" main.go，里面是 package main。"},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
			usage,
			data(`[DONE]`),
		}
	}
}

// toolCallRound 让供应商回放一次工具调用；args 由 encoding/json 编码，
// 避免在脚本里手写转义后的参数串。
func toolCallRound(id, name string, args any) func(func(llm.Chunk) error) error {
	return func(fn func(llm.Chunk) error) error {
		raw, err := json.Marshal(args)
		if err != nil {
			return err
		}
		if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: id, Name: name}}); err != nil {
			return err
		}
		if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: string(raw)}}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopToolUse})
	}
}

// configureFullRuntime 接上全套工具与真实执行后端：阶段 e2e 要从读文件一路
// 走到落盘和跑测试，任何一个工具缺席都会让链路停在半路。
func configureFullRuntime(t *testing.T, a *app.App, provider llm.Provider) string {
	t.Helper()
	return configureFullRuntimeTimeout(t, a, provider, 60*time.Second)
}

// configureFullRuntimeTimeout 与 configureFullRuntime 相同，但可指定执行超时：
// 超时用例必须用秒级窗口，否则测试要跑满一分钟。
func configureFullRuntimeTimeout(t *testing.T, a *app.App, provider llm.Provider, execTimeout time.Duration) string {
	t.Helper()
	return configureFullRuntimeRuleset(t, a, provider, execTimeout, perm.DefaultRuleset())
}

// configureFullRuntimeRuleset 与 configureFullRuntime 相同，但可指定规则集：
// 本地收紧策略用例要走真实的 Loader 产物，才能证明 policy.d 真的生效。
func configureFullRuntimeRuleset(t *testing.T, a *app.App, provider llm.Provider, execTimeout time.Duration, ruleset *perm.Ruleset) string {
	t.Helper()
	outputDir := filepath.Join(t.TempDir(), "outputs")
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(
				tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
				tools.NewWriteFile(ws), tools.NewEditFile(ws),
				tools.NewRunCommand(ws, env), tools.NewCheckCommand(ws, env),
				tools.NewReadOutput(env),
				tools.NewGitStatus(ws, env), tools.NewGitDiff(ws, env), tools.NewGitLog(ws, env),
			)
		},
		Backend:       execpkg.NewDirect(),
		OutputDir:     outputDir,
		Ruleset:       ruleset,
		ExecTimeout:   execTimeout,
		MaxRounds:     6,
		ContextBudget: 20000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	return outputDir
}

// hangTestSource 是一个会跑满两分钟、每 100ms 写一次心跳的测试：超时与取消
// 用例都需要一个「不靠自己结束」的靶子。心跳路径直接写进源码，因为子进程
// 环境是被清洗过的，环境变量传不进去。
func hangTestSource(heartbeat string) string {
	return fmt.Sprintf(`package demo

import (
	"os"
	"testing"
	"time"
)

func TestHang(t *testing.T) {
	f, err := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < 1200; i++ {
		if _, err := f.WriteString("tick\n"); err != nil {
			t.Fatal(err)
		}
		f.Sync()
		time.Sleep(100 * time.Millisecond)
	}
}
`, heartbeat)
}

func TestEndToEndReadOnlyLoop(t *testing.T) {
	ctx := context.Background()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var rounds int32
	srv := fakeOpenAI(t, &rounds)
	defer srv.Close()

	a := newApp(t)
	timeout, retries := 5, 0
	providerCfg := config.Provider{
		Name:           "fake",
		Type:           "openai",
		BaseURL:        srv.URL,
		Model:          "fake-model",
		APIKey:         config.Secret("test-key"),
		TimeoutSeconds: &timeout,
		MaxRetries:     &retries,
	}
	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			p, err := llm.New(providerCfg)
			return p, providerCfg.Model, err
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws))
		},
		MaxRounds:     6,
		ContextBudget: 20000,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	sess, err := a.CreateSession(ctx, dir, "fake", providerCfg.Model, "端到端")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "这仓库根目录有哪些 Go 文件？main.go 里是什么？")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	done := waitTerminal(t, a, task.ID)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s，错误 = %q", done.Status, done.Error)
	}
	if done.PromptTokens == 0 || done.CompleteTokens == 0 {
		t.Fatalf("token 未累加：%+v", done)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	var (
		rounds_   int
		requested []string
		results   []string
		finalText string
		statusSeq []string
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventRoundStarted:
			rounds_++
		case domain.EventToolRequested:
			var p domain.ToolRequestedPayload
			_ = ev.Decode(&p)
			requested = append(requested, p.Tool)
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			_ = ev.Decode(&p)
			results = append(results, p.Output)
		case domain.EventAssistantMessage:
			var p domain.AssistantMessagePayload
			_ = ev.Decode(&p)
			finalText = p.Text
		case domain.EventTaskStatus:
			var p domain.TaskStatusPayload
			_ = ev.Decode(&p)
			statusSeq = append(statusSeq, fmt.Sprintf("%s→%s", p.From, p.To))
		}
	}

	if rounds_ != 3 {
		t.Fatalf("轮数 = %d，期望 3", rounds_)
	}
	if len(requested) != 2 || requested[0] != "glob" || requested[1] != "read_file" {
		t.Fatalf("工具调用序列 = %v，期望 [glob read_file]", requested)
	}
	if len(results) != 2 || !strings.Contains(results[0], "main.go") {
		t.Fatalf("glob 结果未带给模型：%v", results)
	}
	if !strings.Contains(results[1], "package main") {
		t.Fatalf("read_file 结果未带给模型：%v", results)
	}
	if !strings.Contains(finalText, "main.go") {
		t.Fatalf("最终回答 = %q", finalText)
	}
	if got := strings.Join(statusSeq, ","); !strings.Contains(got, "queued→running") || !strings.Contains(got, "running→completed") {
		t.Fatalf("状态迁移序列 = %v", statusSeq)
	}
}

// TestEndToEndWriteApprovalLoop 是 M4 的验收路径：读文件 → 出 diff → 批准 →
// 落盘。diff 必须真实出现在审批载荷里（否则用户是在盲批），落盘内容与提案的
// new_hash 必须一致。拒绝分支见 TestApprovalDenyLeavesDiskUntouched。
func TestEndToEndWriteApprovalLoop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "calc.go"),
		[]byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const testContent = "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"Add(1, 2) != 3\")\n\t}\n}\n"
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		toolCallRound("call_read", "read_file", map[string]any{"path": "calc.go"}),
		toolCallRound("call_write", "write_file", map[string]any{"path": "calc_test.go", "content": testContent}),
		textRound("已新增 calc_test.go"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "写入验收")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "给 Add 函数加测试")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var req domain.ApprovalRequestedPayload
	for _, ev := range events {
		if ev.Type != domain.EventApprovalRequested {
			continue
		}
		if err := ev.Decode(&req); err != nil {
			t.Fatal(err)
		}
	}
	if req.ApprovalID != approvalID {
		t.Fatalf("审批 id = %q，期望 %q", req.ApprovalID, approvalID)
	}
	if req.Kind != domain.ApprovalDiff || req.Path != "calc_test.go" {
		t.Fatalf("审批载荷 = %+v，期望写入 calc_test.go 的 diff", req)
	}
	if req.Truncated {
		t.Fatal("这么小的 diff 不该被截断")
	}
	if !strings.Contains(req.Diff, "+package calc") || !strings.Contains(req.Diff, "+func TestAdd") {
		t.Fatalf("审批 diff 未包含新增内容：\n%s", req.Diff)
	}

	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, ""); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
	}

	got, err := os.ReadFile(filepath.Join(dir, "calc_test.go"))
	if err != nil {
		t.Fatalf("审批通过后应落盘：%v", err)
	}
	if string(got) != testContent {
		t.Fatalf("落盘内容 = %q", got)
	}

	events, err = a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		applied  domain.ProposalAppliedPayload
		writeRes domain.ToolResultPayload
		resolved domain.ApprovalResolvedPayload
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventProposalApplied:
			if err := ev.Decode(&applied); err != nil {
				t.Fatal(err)
			}
		case domain.EventApprovalResolved:
			if err := ev.Decode(&resolved); err != nil {
				t.Fatal(err)
			}
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "write_file" {
				writeRes = p
			}
		}
	}
	if applied.Path != "calc_test.go" || applied.NewHash == "" {
		t.Fatalf("proposal.applied = %+v", applied)
	}
	if resolved.Decision != domain.DecisionAllowOnce || resolved.Source != domain.SourceUser {
		t.Fatalf("approval.resolved = %+v", resolved)
	}
	if writeRes.Status != string(domain.ToolSucceeded) || writeRes.IsError {
		t.Fatalf("write_file 的 tool.result = %+v", writeRes)
	}
}

// TestEndToEndStaleProposal 验证 CAS：审批期间文件被外部改过时，批准也不能
// 覆盖别人的改动。提案转 stale、工具结果返回 stale_proposal、磁盘保留外部内容，
// 任务照常收尾（模型下一轮重新读文件）。
func TestEndToEndStaleProposal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const externalContent = "external change\n"
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		toolCallRound("call_w", "write_file", map[string]any{"path": "out.txt", "content": "approved content"}),
		textRound("文件被改过，没有覆盖"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "CAS")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "改一下 out.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)

	// 用户还在看 diff 时，另一个进程把目标文件改了：批准不能当成「文件没变」。
	if err := os.WriteFile(path, []byte(externalContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, "批准"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("CAS 失败不该让任务失败，终态 = %s（%s）", done.Status, done.Error)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != externalContent {
		t.Fatalf("外部改动被覆盖了：%q", got)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		rejected domain.ProposalRejectedPayload
		resolved domain.ApprovalResolvedPayload
		writeRes domain.ToolResultPayload
		applied  bool
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventProposalApplied:
			applied = true
		case domain.EventProposalRejected:
			if err := ev.Decode(&rejected); err != nil {
				t.Fatal(err)
			}
		case domain.EventApprovalResolved:
			if err := ev.Decode(&resolved); err != nil {
				t.Fatal(err)
			}
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "write_file" {
				writeRes = p
			}
		}
	}
	if applied {
		t.Fatalf("CAS 不匹配时不该有 proposal.applied：%v", kindsOf(events))
	}
	if rejected.Path != "out.txt" || rejected.Status != domain.ProposalStale {
		t.Fatalf("proposal.rejected = %+v，期望 out.txt 的 stale", rejected)
	}
	if !strings.Contains(rejected.Reason, "base_hash") {
		t.Fatalf("rejected 原因应说明 hash 不匹配：%q", rejected.Reason)
	}
	if resolved.Decision != domain.DecisionAllowOnce || resolved.Source != domain.SourceUser {
		t.Fatalf("approval.resolved = %+v", resolved)
	}
	if writeRes.Status != string(domain.ToolFailed) || !writeRes.IsError {
		t.Fatalf("write_file 的 tool.result = %+v，期望 failed/is_error", writeRes)
	}
	if !strings.Contains(writeRes.Output, "stale_proposal") {
		t.Fatalf("模型应收到 stale_proposal：%q", writeRes.Output)
	}
}

// TestEndToEndCancelWhileAwaitingApproval 验证审批不设超时、但取消能把它打断：
// 面板还开着时取消任务，审批必须收尾为 expired（source=system）、提案转 stale、
// 工具调用与 tool.result 都是 canceled，磁盘一个字节都不动。
func TestEndToEndCancelWhileAwaitingApproval(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	const original = "original content\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		toolCallRound("call_w", "write_file", map[string]any{"path": "out.txt", "content": "approved content"}),
		textRound("不该走到这里"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "审批时取消")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "改一下 out.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	waitApproval(t, a, sess.ID)
	if err := a.CancelTask(ctx, sess.ID, task.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCanceled {
		t.Fatalf("终态 = %s（%s），期望 canceled", done.Status, done.Error)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("取消后磁盘被改了：%q", got)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		resolved domain.ApprovalResolvedPayload
		rejected domain.ProposalRejectedPayload
		result   domain.ToolResultPayload
		applied  bool
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventProposalApplied:
			applied = true
		case domain.EventApprovalResolved:
			if err := ev.Decode(&resolved); err != nil {
				t.Fatal(err)
			}
		case domain.EventProposalRejected:
			if err := ev.Decode(&rejected); err != nil {
				t.Fatal(err)
			}
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "write_file" {
				result = p
			}
		}
	}
	if applied {
		t.Fatalf("取消后不该有 proposal.applied：%v", kindsOf(events))
	}
	if resolved.Decision != domain.DecisionExpired || resolved.Source != domain.SourceSystem {
		t.Fatalf("approval.resolved = %+v，期望 expired/system", resolved)
	}
	if rejected.Path != "out.txt" || rejected.Status != domain.ProposalStale {
		t.Fatalf("proposal.rejected = %+v，期望 out.txt 的 stale", rejected)
	}
	if result.Status != string(domain.ToolCanceled) || !result.IsError {
		t.Fatalf("tool.result = %+v，期望 canceled/is_error", result)
	}
	// 投影也得跟着收尾：停在 waiting_approval 的记录会让会话看起来永远在
	// 「等你批准」。
	call, err := a.GetToolCall(ctx, domain.ToolCallID(result.ToolCallID))
	if err != nil {
		t.Fatalf("GetToolCall: %v", err)
	}
	if call.Status != domain.ToolCanceled || call.EndedAt.IsZero() {
		t.Fatalf("tool_call = %+v，期望 canceled 且有结束时间", call)
	}
}

// TestEndToEndCancelRunningCommand 验证命令执行中被取消：进程组整体被杀，
// 任务转 canceled，工具结果也是 canceled。用一个每 100ms 写心跳的测试进程
// 作靶子——取消后心跳停止，比只看任务状态更能证明子进程真的死了。
// skipPOSIXCommands 跳过依赖 sh/cat 等 POSIX 可执行文件的用例：Windows 上
// 这些命令不存在，失败与产品行为无关。
func skipPOSIXCommands(t *testing.T) {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip("依赖 POSIX 命令（sh/cat），Windows 跳过")
	}
}

func TestEndToEndCancelRunningCommand(t *testing.T) {
	skipPOSIXCommands(t)
	ctx := context.Background()
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat.txt")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/hang\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.go"),
		[]byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 靶子进程跑得比 ExecTimeout 短，不靠超时收尾：只有取消能把它结束。
	if err := os.WriteFile(filepath.Join(dir, "demo_test.go"), []byte(hangTestSource(heartbeat)), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_run", []string{"go", "test", "./..."}),
		textRound("不该走到这里"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "执行中取消")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "跑一下测试")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 心跳文件出现 = 编译完成、靶子进程已在跑，此时取消才落在执行中间。
	deadline := time.Now().Add(60 * time.Second)
	for {
		if st, err := os.Stat(heartbeat); err == nil && st.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("命令没有开始执行：没有出现心跳文件")
		}
		time.Sleep(50 * time.Millisecond)
	}

	start := time.Now()
	if err := a.CancelTask(ctx, sess.ID, task.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if done := waitTerminalIn(t, a, task.ID, 30*time.Second); done.Status != domain.TaskCanceled {
		t.Fatalf("终态 = %s（%s），期望 canceled", done.Status, done.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("取消耗时 %s，像是等到了命令自己结束或被超时带走", elapsed)
	}

	// 心跳停下来才能证明子进程（go test 及其测试二进制）整体被杀，
	// 而不只是留下了一个还在后台写文件的孤儿。
	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("取消后靶子进程还在写心跳：%d → %d 字节", before.Size(), after.Size())
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		result   domain.ToolResultPayload
		approval bool
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventApprovalRequested:
			approval = true
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "run_command" {
				result = p
			}
		}
	}
	if approval {
		t.Fatal("白名单命令不该发起审批")
	}
	if result.Tool != "run_command" {
		t.Fatalf("缺少 run_command 的 tool.result：%v", kindsOf(events))
	}
	if result.Status != string(domain.ToolCanceled) || !result.IsError {
		t.Fatalf("tool.result = %+v，期望 canceled/is_error", result)
	}
	if strings.Contains(result.Output, "ok") {
		t.Fatalf("被取消的命令不该有测试通过输出：%q", result.Output)
	}
}

// TestEndToEndPolicyTightenGitStatus 验证本地 policy.d 收紧规则压过只读短路：
// git_status 默认自动放行，收紧后必须落到审批，拒绝时进程一个字节都不启动。
func TestEndToEndPolicyTightenGitStatus(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 走真实的 Loader：外部文件只能收紧，这条用例不手工拼 Ruleset。
	policyDir := t.TempDir()
	const policy = `version: test-tighten
rules:
  - id: tighten.git_status
    tool: git_status
    programs: [git]
    subcommands: [status]
    reason: 本地策略要求审批
`
	if err := os.WriteFile(filepath.Join(policyDir, "20-git.yaml"), []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	ruleset, err := perm.NewLoader(policyDir).Load()
	if err != nil {
		t.Fatalf("加载本地策略: %v", err)
	}
	// Loader 把外部策略版本拼在内嵌版本之后。
	if want := perm.DefaultRuleset().Version + "+test-tighten"; ruleset.Version != want {
		t.Fatalf("规则版本 = %q，期望 %q", ruleset.Version, want)
	}

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		toolCallRound("call_git", "git_status", map[string]any{}),
		textRound("好，不查了"),
	}}
	a := newApp(t)
	configureFullRuntimeRuleset(t, a, provider, 60*time.Second, ruleset)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "策略收紧")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "看看 git 状态")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 审批面板出现就说明只读短路没有直接放行。
	approvalID := waitApproval(t, a, sess.ID)
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var req domain.ApprovalRequestedPayload
	for _, ev := range events {
		if ev.Type != domain.EventApprovalRequested {
			continue
		}
		if err := ev.Decode(&req); err != nil {
			t.Fatal(err)
		}
	}
	if req.Kind != domain.ApprovalCommand {
		t.Fatalf("审批 kind = %q，期望 command", req.Kind)
	}
	if want := "git status --porcelain=v1 --untracked-files=normal"; strings.Join(req.Command, " ") != want {
		t.Fatalf("审批 argv = %v，期望 %s", req.Command, want)
	}
	// 原因必须指向 tighten，而不是 not_whitelisted：后者说明规则根本没生效。
	if !strings.Contains(req.Reason, "tighten") {
		t.Fatalf("审批原因 = %q，期望来自本地收紧规则", req.Reason)
	}

	const note = "本地策略要求审批，不查"
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionDeny, note); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("拒绝不是任务失败，终态 = %s（%s）", done.Status, done.Error)
	}

	events, err = a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result domain.ToolResultPayload
	for _, ev := range events {
		if ev.Type != domain.EventToolResult {
			continue
		}
		var p domain.ToolResultPayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Tool == "git_status" {
			result = p
		}
	}
	if result.Status != string(domain.ToolDenied) || !result.IsError {
		t.Fatalf("tool.result = %+v，期望 denied/is_error", result)
	}
	if !strings.Contains(result.Output, note) {
		t.Fatalf("拒绝理由应回给模型：%q", result.Output)
	}

	audit, err := a.RecentAudit(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("审计条数 = %d，期望 1：%+v", len(audit), audit)
	}
	if audit[0].Action != "exec.denied" || audit[0].Actor != "user" {
		t.Fatalf("审计 = %+v，期望 exec.denied/user", audit[0])
	}
	var detail map[string]any
	if err := json.Unmarshal(audit[0].Detail, &detail); err != nil {
		t.Fatalf("审计详情: %v", err)
	}
	if detail["rule_id"] != "tighten.git_status" || detail["policy_version"] != ruleset.Version {
		t.Fatalf("审计未记录命中的本地规则：%+v", detail)
	}
}

// TestEndToEndReadOutputRecoversTruncated 验证「截断 → 补看」闭环：run_command
// 的事件载荷只留头尾，中段只能靠 read_output 从全文日志里按行号取回来。
func TestEndToEndReadOutputRecoversTruncated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 2000 行约 88 KB：远超 8 KB 的事件上限，中段一定会被丢掉。
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&b, "line %04d alpha beta gamma delta epsilon\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_cat", []string{"cat", "big.txt"}),
		toolCallRound("call_read", "read_output", map[string]any{
			"tool_call_id": "call_cat", "offset": 995, "limit": 10,
		}),
		textRound("中段已补看"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "补看全文")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "看看 big.txt 的内容")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	if done := waitTerminalIn(t, a, task.ID, 30*time.Second); done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		catResult  domain.ToolResultPayload
		readResult domain.ToolResultPayload
		approval   bool
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventApprovalRequested:
			approval = true
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			switch p.Tool {
			case "run_command":
				catResult = p
			case "read_output":
				readResult = p
			}
		}
	}
	if approval {
		t.Fatal("cat 在白名单内，不该发起审批")
	}

	if !catResult.Truncated {
		t.Fatal("88 KB 的输出必须标记截断")
	}
	if strings.Contains(catResult.Output, "line 1000") {
		t.Fatal("中段行不该留在事件载荷里，否则这条用例验证不了补看")
	}
	if !strings.Contains(catResult.Output, "已截断") {
		t.Fatalf("截断载荷应带提示：%q", catResult.Output)
	}
	if catResult.OutputPath == "" {
		t.Fatal("run_command 应落全文日志并给出 output_path")
	}
	if _, err := os.Stat(catResult.OutputPath); err != nil {
		t.Fatalf("全文日志不存在：%v", err)
	}

	if readResult.Tool != "read_output" || readResult.IsError {
		t.Fatalf("read_output 结果 = %+v", readResult)
	}
	if !strings.Contains(readResult.Output, "日志共 2000 行") {
		t.Fatalf("补看结果应报总行数：%q", readResult.Output)
	}
	if !strings.Contains(readResult.Output, "1000: line 1000 alpha") {
		t.Fatalf("补看结果未取回被截断的中段：%q", readResult.Output)
	}
	// 窗口后面还有行，read_output 会提示可以继续补看：这是分页而不是失败。
	if !strings.Contains(readResult.Output, "显示 995-1004") {
		t.Fatalf("补看窗口不对：%q", readResult.Output)
	}

	// 最终回答必须基于补看回来的内容，而不是只看到了头尾。
	var finalText string
	for _, ev := range events {
		if ev.Type != domain.EventAssistantMessage {
			continue
		}
		var p domain.AssistantMessagePayload
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		finalText = p.Text
	}
	if !strings.Contains(finalText, "补看") {
		t.Fatalf("最终回答 = %q", finalText)
	}
}

// TestEndToEndCommandTimeout 验证执行超时：进程组被整体杀掉，审计记
// exec.timeout（actor=system）而不是 exec.failed，任务照常收尾。
func TestEndToEndCommandTimeout(t *testing.T) {
	skipPOSIXCommands(t)
	ctx := context.Background()
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat.txt")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/hang\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.go"),
		[]byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo_test.go"), []byte(hangTestSource(heartbeat)), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_run", []string{"go", "test", "./..."}),
		textRound("命令超时了"),
	}}
	a := newApp(t)
	configureFullRuntimeTimeout(t, a, provider, 8*time.Second)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "执行超时")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "跑一下测试")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 先等心跳出现，确认靶子进程真的在跑：否则「超时被杀」的断言可能是
	// 在编译阶段就超时了，根本没机会验证进程组。
	start := time.Now()
	headbeatDeadline := time.Now().Add(8 * time.Second)
	for {
		if st, err := os.Stat(heartbeat); err == nil && st.Size() > 0 {
			break
		}
		if time.Now().After(headbeatDeadline) {
			t.Fatal("靶子进程没启动（心跳文件缺失），无法验证超时杀掉进程组")
		}
		time.Sleep(50 * time.Millisecond)
	}

	done := waitTerminalIn(t, a, task.ID, 60*time.Second)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("超时不是任务失败，终态 = %s（%s）", done.Status, done.Error)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("超时收敛用了 %s，像是等到了命令自己结束", elapsed)
	}

	// 心跳停下来才能证明进程组被杀，而不只是主进程退出、留下孤儿。
	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("超时后靶子进程还在写心跳：%d → %d 字节", before.Size(), after.Size())
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		result   domain.ToolResultPayload
		approval bool
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventApprovalRequested:
			approval = true
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "run_command" {
				result = p
			}
		}
	}
	if approval {
		t.Fatal("白名单命令不该发起审批")
	}
	if result.Status != string(domain.ToolFailed) || !result.IsError {
		t.Fatalf("tool.result = %+v，期望 failed/is_error", result)
	}
	if !strings.Contains(result.Output, "命令超时被终止") {
		t.Fatalf("超时结果应告诉模型超时了：%q", result.Output)
	}

	audit, err := a.RecentAudit(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("审计条数 = %d，期望 1：%+v", len(audit), audit)
	}
	if audit[0].Action != "exec.timeout" || audit[0].Actor != "system" {
		t.Fatalf("审计 = %+v，期望 exec.timeout/system", audit[0])
	}
}

// TestEndToEndDeniedCommandNeverRuns 验证命令审批拒绝分支：命令不在白名单
// 里 → 发起审批 → 用户拒绝，进程一个字节都不启动（靶子命令是 touch，跑过就会
// 留下文件），审计记 exec.denied 而不是执行失败。
func TestEndToEndDeniedCommandNeverRuns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")

	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_c", []string{"touch", "marker.txt"}),
		textRound("好，不执行"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "命令拒绝")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "建个标记文件")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var req domain.ApprovalRequestedPayload
	for _, ev := range events {
		if ev.Type != domain.EventApprovalRequested {
			continue
		}
		if err := ev.Decode(&req); err != nil {
			t.Fatal(err)
		}
	}
	if req.Kind != domain.ApprovalCommand || strings.Join(req.Command, " ") != "touch marker.txt" {
		t.Fatalf("审批载荷 = %+v，期望 touch marker.txt 的命令审批", req)
	}
	// macOS 上 /var 是 /private/var 的符号链接：工作区会把 cwd 解析成真实路径。
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if req.Cwd != realDir {
		t.Fatalf("审批 cwd = %q，期望 %q", req.Cwd, realDir)
	}

	const note = "别动工作区"
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionDeny, note); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("拒绝不是任务失败，终态 = %s（%s）", done.Status, done.Error)
	}

	// touch 跑过就一定会留下文件：文件不存在才等价于「没启动进程」。
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("拒绝后不该产生文件，stat 错误 = %v", err)
	}

	events, err = a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		resolved domain.ApprovalResolvedPayload
		result   domain.ToolResultPayload
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventApprovalResolved:
			if err := ev.Decode(&resolved); err != nil {
				t.Fatal(err)
			}
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "run_command" {
				result = p
			}
		}
	}
	if resolved.Decision != domain.DecisionDeny || resolved.Source != domain.SourceUser {
		t.Fatalf("approval.resolved = %+v，期望 deny/user", resolved)
	}
	if result.Status != string(domain.ToolDenied) || !result.IsError {
		t.Fatalf("tool.result = %+v，期望 denied/is_error", result)
	}
	if !strings.Contains(result.Output, note) {
		t.Fatalf("拒绝理由应回给模型：%q", result.Output)
	}

	audit, err := a.RecentAudit(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("审计条数 = %d，期望 1：%+v", len(audit), audit)
	}
	if audit[0].Action != "exec.denied" || audit[0].Actor != "user" {
		t.Fatalf("审计 = %+v，期望 exec.denied/user", audit[0])
	}
	var detail map[string]any
	if err := json.Unmarshal(audit[0].Detail, &detail); err != nil {
		t.Fatalf("审计详情: %v", err)
	}
	argv, _ := detail["argv"].([]any)
	if len(argv) != 2 || argv[0] != "touch" || argv[1] != "marker.txt" {
		t.Fatalf("审计 argv = %v", argv)
	}
}

// TestEndToEndAllowSessionSkipsSecondCommandApproval 验证会话级放行的完整闭环：
// 同参数命令第一次停在审批面板，选了 allow_session 后第二个任务不再等人工裁决
// （没有调用 ResolveApproval 也能收敛），但两次都真的执行，且第二次的放行在
// 事件里记成 policy 而不是又一个 user 裁决。
func TestEndToEndAllowSessionSkipsSecondCommandApproval(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// cat -u 是合法参数但不在允许 flag 列表里：一定落到审批，而且两次执行的
	// 输出可核对（BSD 与 GNU cat 都支持 -u，不依赖平台）。
	argv := []string{"cat", "-u", "notes.txt"}
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_cat1", argv),
		textRound("第一次看过"),
		runCommandRound("call_cat2", argv),
		textRound("第二次没再问"),
	}}
	a := newApp(t)
	configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "会话放行")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, err := a.SubmitTask(ctx, sess.ID, "看看 notes.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)
	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var req domain.ApprovalRequestedPayload
	for _, ev := range events {
		if ev.Type != domain.EventApprovalRequested {
			continue
		}
		if err := ev.Decode(&req); err != nil {
			t.Fatal(err)
		}
	}
	if req.Kind != domain.ApprovalCommand {
		t.Fatalf("审批 kind = %q，期望 command", req.Kind)
	}
	if strings.Join(req.Command, " ") != strings.Join(argv, " ") {
		t.Fatalf("审批 argv = %v，期望 %v", req.Command, argv)
	}
	// 原因指向 flag 不在允许列表，说明确实走到了参数校验而不是别的分支。
	if !strings.Contains(req.Reason, "unsafe_args") {
		t.Fatalf("审批原因 = %q，期望 unsafe_args", req.Reason)
	}

	const note = "这个参数以后不用问"
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowSession, note); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminalIn(t, a, first.ID, 15*time.Second); done.Status != domain.TaskCompleted {
		t.Fatalf("第一个任务终态 = %s（%s）", done.Status, done.Error)
	}

	// 第二个任务不调用 ResolveApproval：命中会话白名单才能收敛；否则它会一直
	// 停在审批面板上，等不到终态而超时失败。
	second, err := a.SubmitTask(ctx, sess.ID, "再看一眼 notes.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if done := waitTerminalIn(t, a, second.ID, 15*time.Second); done.Status != domain.TaskCompleted {
		t.Fatalf("第二个任务终态 = %s（%s），像是又弹了一次审批", done.Status, done.Error)
	}

	events, err = a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		requested int
		resolved  []domain.ApprovalResolvedPayload
		runs      []domain.ToolResultPayload
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventApprovalRequested:
			requested++
		case domain.EventApprovalResolved:
			var p domain.ApprovalResolvedPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			resolved = append(resolved, p)
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "run_command" {
				runs = append(runs, p)
			}
		}
	}
	// 每次命令调用都照常记 approval.requested（放行也要留痕），所以两条都算；
	// 「不再重复审批」的证据是第二个任务没等人裁决就收敛了。
	if requested != 2 {
		t.Fatalf("approval.requested 条数 = %d，期望 2", requested)
	}
	if len(resolved) != 2 {
		t.Fatalf("approval.resolved 条数 = %d，期望 2", len(resolved))
	}
	bySource := map[domain.ApprovalSource]domain.ApprovalResolvedPayload{}
	for _, p := range resolved {
		if p.Decision != domain.DecisionAllowSession {
			t.Fatalf("approval.resolved = %+v，期望 allow_session", p)
		}
		bySource[p.Source] = p
	}
	// 第二次是策略放行，不能伪装成又一次人工点击。
	if _, ok := bySource[domain.SourceUser]; !ok {
		t.Fatalf("缺少 user 裁决：%+v", resolved)
	}
	if _, ok := bySource[domain.SourcePolicy]; !ok {
		t.Fatalf("缺少 policy 放行：%+v", resolved)
	}

	// 不再审批不等于跳过执行：两次都必须真的跑出文件内容。
	if len(runs) != 2 {
		t.Fatalf("run_command 结果 = %d 条，期望 2：%+v", len(runs), runs)
	}
	for i, r := range runs {
		if r.Status != string(domain.ToolSucceeded) || r.IsError {
			t.Fatalf("第 %d 次 run_command = %+v，期望 succeeded", i+1, r)
		}
		if !strings.Contains(r.Output, "hello") {
			t.Fatalf("第 %d 次没看到文件内容：%q", i+1, r.Output)
		}
	}

	// 审计的 actor 要跟审批来源一致：第一次是人工点击，第二次是会话策略
	// 自动放行，不能把策略放行写成用户的决定。
	assertExecApprovedBy(t, ctx, a, first.ID, "user")
	assertExecApprovedBy(t, ctx, a, second.ID, "policy")
}

// assertExecApprovedBy 断言任务恰好留下一条 exec.approved 审计，且 actor 是
// 期望的审批来源。
func assertExecApprovedBy(t *testing.T, ctx context.Context, a *app.App, taskID domain.TaskID, actor string) {
	t.Helper()
	audit, err := a.RecentAudit(ctx, taskID, 10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(audit) != 1 {
		t.Fatalf("任务 %s 审计条数 = %d，期望 1：%+v", taskID, len(audit), audit)
	}
	if audit[0].Action != "exec.approved" || audit[0].Actor != actor {
		t.Fatalf("任务 %s 审计 = %+v，期望 exec.approved/%s", taskID, audit[0], actor)
	}
}

// TestEndToEndWriteApproveThenRunTest 是 v1 的完整验收链路：读文件 → 出 diff →
// 批准落盘 → 跑 `go test ./...` → 回显结果。它把 M4 与 M5 接在一起，验证
// 落盘的事件先于执行，且执行结果真的回到了模型与事件流里。
func TestEndToEndWriteApproveThenRunTest(t *testing.T) {
	skipPOSIXCommands(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/demo\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.go"),
		[]byte("package demo\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const testContent = "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"Add(1, 2) != 3\")\n\t}\n}\n"
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		toolCallRound("call_read", "read_file", map[string]any{"path": "demo.go"}),
		toolCallRound("call_write", "write_file", map[string]any{"path": "demo_test.go", "content": testContent}),
		runCommandRound("call_run", []string{"go", "test", "./..."}),
		textRound("已加测试并通过"),
	}}
	a := newApp(t)
	outputDir := configureFullRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", "test-model", "完整验收")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "给 Add 加测试并跑一遍")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	approvalID := waitApproval(t, a, sess.ID)
	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, "可以"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	// 窗口长于 ExecTimeout（60s）：命令真超时时应由 exec 给出带状态的终态，
	// 而不是在这里报「还没结束」。
	done := waitTerminalIn(t, a, task.ID, 90*time.Second)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s（%q）", done.Status, done.Error)
	}

	got, err := os.ReadFile(filepath.Join(dir, "demo_test.go"))
	if err != nil {
		t.Fatalf("审批通过后应落盘：%v", err)
	}
	if string(got) != testContent {
		t.Fatalf("落盘内容 = %q", got)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var (
		requested  []string
		requestSeq = map[string]domain.Seq{}
		appliedSeq domain.Seq
		runResult  domain.ToolResultPayload
		finalText  string
		applied    bool
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventToolRequested:
			var p domain.ToolRequestedPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			requested = append(requested, p.Tool)
			requestSeq[p.Tool] = ev.Seq
		case domain.EventProposalApplied:
			applied = true
			appliedSeq = ev.Seq
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Tool == "run_command" {
				runResult = p
			}
		case domain.EventAssistantMessage:
			var p domain.AssistantMessagePayload
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			finalText = p.Text
		}
	}

	if want := "read_file,write_file,run_command"; strings.Join(requested, ",") != want {
		t.Fatalf("工具调用序列 = %v，期望 %s", requested, want)
	}
	if !applied {
		t.Fatalf("缺少 proposal.applied：%v", kindsOf(events))
	}
	if appliedSeq >= requestSeq["run_command"] {
		t.Fatalf("落盘（seq %d）应先于执行（seq %d）", appliedSeq, requestSeq["run_command"])
	}
	if runResult.Tool != "run_command" {
		t.Fatalf("run_command 结果缺失：%+v", runResult)
	}
	if runResult.IsError || runResult.ExitCode == nil || *runResult.ExitCode != 0 {
		t.Fatalf("tool.result = %+v", runResult)
	}
	if !strings.Contains(runResult.Output, "ok") {
		t.Fatalf("测试结果未回显：%q", runResult.Output)
	}
	if runResult.OutputPath == "" {
		t.Fatal("tool.result 应带 output_path")
	}
	if !strings.HasPrefix(runResult.OutputPath, outputDir) {
		t.Fatalf("日志应落在 %s 之内，实际 %s", outputDir, runResult.OutputPath)
	}
	if _, err := os.Stat(runResult.OutputPath); err != nil {
		t.Fatalf("全文日志应落盘：%v", err)
	}
	if !strings.Contains(finalText, "通过") {
		t.Fatalf("最终回答 = %q，应基于命令结果", finalText)
	}

	audit, err := a.RecentAudit(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var allowed bool
	for _, record := range audit {
		if record.Action == "exec.allow" && record.Actor == "policy" {
			allowed = true
		}
	}
	if !allowed {
		t.Fatalf("白名单执行应留 exec.allow 审计：%+v", audit)
	}
}
