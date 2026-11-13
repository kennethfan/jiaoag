package app_test

import (
	"context"
	"encoding/json"
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

// fakeCommandOpenAI 回放三轮：run_command(go test ./...) → read_output(补看全文) → 结论。
func fakeCommandOpenAI(t *testing.T, rounds *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("读取请求体: %v", err)
		}
		round := atomic.AddInt32(rounds, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		data := func(v any) string {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Errorf("编码帧: %v", err)
				return ""
			}
			return "data: " + string(raw) + "\n\n"
		}
		usage := data(map[string]any{
			"choices": []any{},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 4},
		})

		toolFrame := func(id, name string, args any) string {
			raw, _ := json.Marshal(args)
			return data(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(raw)},
				}}},
				"finish_reason": nil,
			}}})
		}

		var frames []string
		switch round {
		case 1:
			frames = []string{
				toolFrame("call_run", "run_command", map[string]any{"argv": []string{"go", "test", "./..."}}),
				data(map[string]any{"choices": []any{map[string]any{
					"delta": map[string]any{}, "finish_reason": "tool_calls",
				}}}),
			}
		case 2:
			frames = []string{
				toolFrame("call_read", "read_output", map[string]any{"tool_call_id": "call_run"}),
				data(map[string]any{"choices": []any{map[string]any{
					"delta": map[string]any{}, "finish_reason": "tool_calls",
				}}}),
			}
		default:
			frames = []string{
				data(map[string]any{"choices": []any{map[string]any{
					"delta": map[string]any{"content": "测试通过了。"}, "finish_reason": nil,
				}}}),
				data(map[string]any{"choices": []any{map[string]any{
					"delta": map[string]any{}, "finish_reason": "stop",
				}}}),
			}
		}
		// [DONE] 是 SSE 哨兵，不能当 JSON 编码。
		frames = append(frames, usage, "data: [DONE]\n\n")
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

// configureCommandRuntime 接上真实执行后端与默认规则集。
func configureCommandRuntime(t *testing.T, a *app.App, provider llm.Provider) string {
	t.Helper()
	outputDir := filepath.Join(t.TempDir(), "outputs")
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(
				tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
				tools.NewRunCommand(ws, env), tools.NewCheckCommand(ws, env),
				tools.NewReadOutput(env),
				tools.NewGitStatus(ws, env), tools.NewGitDiff(ws, env), tools.NewGitLog(ws, env),
			)
		},
		Backend:       execpkg.NewDirect(),
		Ruleset:       perm.DefaultRuleset(),
		OutputDir:     outputDir,
		ExecTimeout:   60 * time.Second,
		MaxRounds:     4,
		ContextBudget: 20000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	return outputDir
}

// TestEndToEndWhitelistedCommand 是 M5 的验收路径：真实执行后端跑
// `go test ./...`，不打断用户，输出回显，再用 read_output 从落盘全文补看。
func TestEndToEndWhitelistedCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo_test.go"),
		[]byte("package demo\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var rounds int32
	srv := fakeCommandOpenAI(t, &rounds)
	defer srv.Close()

	timeout, retries := 10, 0
	providerCfg := config.Provider{
		Name:           "fake",
		Type:           "openai",
		BaseURL:        srv.URL,
		Model:          "fake-model",
		APIKey:         config.Secret("test-key"),
		TimeoutSeconds: &timeout,
		MaxRetries:     &retries,
	}
	provider, err := llm.New(providerCfg)
	if err != nil {
		t.Fatalf("构造供应商: %v", err)
	}

	a := newApp(t)
	outputDir := configureCommandRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, dir, "fake", providerCfg.Model, "执行验收")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "跑一下这个仓库的测试")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// 窗口长于 ExecTimeout（60s）：命令真超时时应由 exec 给出带状态的终态，
	// 而不是在这里报「还没结束」。
	done := waitTerminalIn(t, a, task.ID, 90*time.Second)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s（%q）", done.Status, done.Error)
	}
	if done.ExtensionsDigest == "" {
		t.Fatal("extensions_digest 应包含规则集与后端")
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var runResult, readResult domain.ToolResultPayload
	for _, ev := range events {
		switch ev.Type {
		case domain.EventApprovalRequested:
			t.Fatal("白名单命令不应发起审批")
		case domain.EventToolResult:
			var r domain.ToolResultPayload
			if err := ev.Decode(&r); err != nil {
				t.Fatal(err)
			}
			switch r.Tool {
			case "run_command":
				runResult = r
			case "read_output":
				readResult = r
			}
		}
	}
	if runResult.Tool != "run_command" {
		t.Fatalf("run_command 结果缺失：%+v", runResult)
	}
	if runResult.IsError || runResult.ExitCode == nil || *runResult.ExitCode != 0 {
		t.Fatalf("tool.result = %+v", runResult)
	}
	if !strings.Contains(runResult.Output, "ok") {
		t.Fatalf("命令输出未回显：%q", runResult.Output)
	}
	if runResult.OutputPath == "" {
		t.Fatal("tool.result 应带 output_path")
	}
	if _, err := os.Stat(runResult.OutputPath); err != nil {
		t.Fatalf("全文日志应落盘：%v", err)
	}
	if !strings.HasPrefix(runResult.OutputPath, outputDir) {
		t.Fatalf("日志应落在 %s 之内，实际 %s", outputDir, runResult.OutputPath)
	}
	// 这一步同时验证 output_path 真的写进了 tool_calls：read_output 只按
	// tool_call_id 反查，查不到就拿不到全文。
	if readResult.Tool != "read_output" || readResult.IsError {
		t.Fatalf("read_output 未取到全文：%+v", readResult)
	}
	if !strings.Contains(readResult.Output, "ok") {
		t.Fatalf("read_output 内容 = %q", readResult.Output)
	}

	audit, err := a.RecentAudit(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("审计条数 = %d，期望 1：%+v", len(audit), audit)
	}
	if audit[0].Action != "exec.allow" || audit[0].Actor != "policy" {
		t.Fatalf("审计 = %+v", audit[0])
	}
	var detail map[string]any
	if err := json.Unmarshal(audit[0].Detail, &detail); err != nil {
		t.Fatalf("审计 detail: %v", err)
	}
	if detail["policy_version"] == "" || detail["backend"] != "direct" {
		t.Fatalf("审计 detail 缺规则版本或后端：%s", audit[0].Detail)
	}
	if v, _ := detail["backend_version"].(string); !strings.HasPrefix(v, "direct/") {
		t.Fatalf("审计 detail 缺后端版本：%s", audit[0].Detail)
	}
	if argv, _ := detail["argv"].([]any); len(argv) != 3 || argv[0] != "go" {
		t.Fatalf("审计 argv = %v", detail["argv"])
	}
}

func runCommandRound(id string, argv []string) func(func(llm.Chunk) error) error {
	return func(fn func(llm.Chunk) error) error {
		args, err := json.Marshal(map[string]any{"argv": argv})
		if err != nil {
			return err
		}
		if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ID: id, Name: "run_command"}}); err != nil {
			return err
		}
		if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{Index: 0, ArgsFragment: string(args)}}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopToolUse})
	}
}

// TestCommandApprovalUsesRealStore 走一遍真实库的命令审批：审批行 kind=command、
// tool_call 阻塞在 waiting_approve、裁决后执行并写 exec.approved 审计。
func TestCommandApprovalUsesRealStore(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	provider := &roundProvider{rounds: []func(func(llm.Chunk) error) error{
		runCommandRound("call_rm", []string{"rm", "-rf", "build"}),
		textRound("没有删除"),
	}}
	configureCommandRuntime(t, a, provider)

	sess, err := a.CreateSession(ctx, t.TempDir(), "p", "m", "命令审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "删掉 build")
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
		t.Fatalf("审批 kind = %q", req.Kind)
	}
	if len(req.Command) != 3 || req.Cwd == "" || req.Backend != "direct" || req.TimeoutMS <= 0 {
		t.Fatalf("审批载荷缺少 argv/cwd/backend/timeout：%+v", req)
	}

	if err := a.ResolveApproval(ctx, sess.ID, approvalID, domain.DecisionAllowOnce, ""); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if done := waitTerminal(t, a, task.ID); done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
	}

	audit, err := a.RecentAudit(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var approved bool
	for _, record := range audit {
		if record.Action != "exec.approved" {
			continue
		}
		approved = true
		if record.Actor != "user" {
			t.Fatalf("exec.approved 的 actor = %q", record.Actor)
		}
		var detail map[string]any
		if err := json.Unmarshal(record.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		if detail["argv"] == nil || detail["backend"] != "direct" {
			t.Fatalf("审计 detail = %s", record.Detail)
		}
	}
	if !approved {
		t.Fatalf("缺少 exec.approved 审计：%+v", audit)
	}
}
