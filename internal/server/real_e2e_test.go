//go:build realprovider

// 真实供应商端到端验收：默认不编译、不进 CI。
//
//	go test -tags realprovider -run TestRealProvider -v ./internal/server/
//
// 需要三个环境变量（缺任一则跳过）：
//
//	JIAOAG_E2E_BASE_URL  OpenAI 兼容端点，例如 https://api.deepseek.com/v1
//	JIAOAG_E2E_API_KEY   密钥（只在进程内存里使用，绝不写盘）
//	JIAOAG_E2E_MODEL     模型名
//	JIAOAG_E2E_PROVIDER  可选，openai（默认）或 anthropic
//
// 这三条用例验证的是「适配器 + 工具循环 + 审批」在真实流量下不回归：
// 只读、diff 审批写盘、命令审批执行。
//
// 本文件带构建标签，默认的 `go vet ./...` 看不到它：改动 internal/app、
// internal/tools、internal/llm 的公开结构后，请额外跑
//
//	go vet -tags realprovider ./internal/server/
package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// frameWait 是「两帧之间」的等待上限：真实模型的首字可能几十秒才到。
const frameWait = 120 * time.Second

func realProviderConfig(t *testing.T) config.Provider {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("JIAOAG_E2E_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("JIAOAG_E2E_API_KEY"))
	model := strings.TrimSpace(os.Getenv("JIAOAG_E2E_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("需要 JIAOAG_E2E_BASE_URL / JIAOAG_E2E_API_KEY / JIAOAG_E2E_MODEL")
	}
	typ := strings.TrimSpace(os.Getenv("JIAOAG_E2E_PROVIDER"))
	if typ == "" {
		typ = "openai"
	}
	return config.Provider{Name: "e2e", Type: typ, BaseURL: base, Model: model, APIKey: config.Secret(key)}
}

// configureRealRuntime 用生产同款接线：完整工具集、默认策略、direct 后端。
func configureRealRuntime(t *testing.T, a *app.App, provider config.Provider) {
	t.Helper()
	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			built, err := llm.New(provider)
			if err != nil {
				return nil, "", err
			}
			return built, provider.Model, nil
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
		Ruleset:       perm.DefaultRuleset(),
		OutputDir:     filepath.Join(t.TempDir(), "outputs"),
		ExecTimeout:   60 * time.Second,
		MaxRounds:     8,
		ContextBudget: 60000,
		// 真实模型超时、适配器重试、工具拒绝都只在这里可见：吞掉日志会让
		// 失败只剩一串事件名。级别压到 Warn，正常轮次不刷屏。
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// realRun 收集一次任务的全部事件，供断言用。
type realRun struct {
	kinds       []string
	approvals   []domain.ApprovalRequestedPayload
	toolResults []domain.ToolResultPayload
	applied     []domain.ProposalAppliedPayload
	rejected    []domain.ProposalRejectedPayload
	assistant   []string
	terminal    domain.TaskStatusPayload
}

func (r *realRun) count(kind domain.EventKind) int {
	n := 0
	for _, k := range r.kinds {
		if k == string(kind) {
			n++
		}
	}
	return n
}

func (r *realRun) toolResult(tool string) (domain.ToolResultPayload, bool) {
	for _, res := range r.toolResults {
		if res.Tool == tool {
			return res, true
		}
	}
	return domain.ToolResultPayload{}, false
}

// runRealTask 提交任务、订阅事件、自动批准所有审批，直到任务进入终态。
func runRealTask(t *testing.T, wsURL string, application *app.App, sessionID domain.SessionID, prompt string) *realRun {
	t.Helper()
	// 整轮截止：帧间超时挡不住「模型一直吐 delta 却不收尾」的情况，
	// 那种场景会让整包测试撞上 go test 的默认超时。
	const taskBudget = 5 * time.Minute
	deadline := time.Now().Add(taskBudget)

	// SubmitTask 立即返回，不跟随任何超时 ctx；进度完全由下面的循环兜底。
	task, err := application.SubmitTask(context.Background(), sessionID, prompt)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	conn := dial(t, wsURL, testToken)
	handshake(t, conn)
	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sessionID),
		FromSeq:   0,
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)

	run := &realRun{}
	for {
		wait := frameWait
		if left := time.Until(deadline); left < wait {
			wait = left
		}
		env, ok := readEnvelopeWithin(conn, wait)
		if !ok {
			if time.Now().After(deadline) {
				t.Fatalf("任务 %s 未在 %s 内完成；已收到：%v", task.ID, taskBudget, run.kinds)
			}
			t.Fatalf("等待任务 %s 的事件超时（%s）；已收到：%v", task.ID, frameWait, run.kinds)
		}
		switch env.Type {
		case protocol.TypeError:
			var p protocol.ErrorPayload
			_ = env.Decode(&p)
			t.Fatalf("服务端错误 %s：%s", p.Code, p.Message)
		case protocol.TypeEvent:
			var p protocol.StreamEventPayload
			if err := env.Decode(&p); err != nil {
				t.Fatalf("解码事件信封: %v", err)
			}
			run.kinds = append(run.kinds, p.Kind)
			switch domain.EventKind(p.Kind) {
			case domain.EventApprovalRequested:
				req := decodeEvent[domain.ApprovalRequestedPayload](t, p)
				run.approvals = append(run.approvals, req)
				// 只批准本任务的审批：将来同会话预置多个任务时，
				// 不能把排队任务的待审批也一起放行。
				if p.TaskID != string(task.ID) {
					continue
				}
				resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
					SessionID:  string(sessionID),
					ApprovalID: string(req.ApprovalID),
					Decision:   string(domain.DecisionAllowOnce),
					Note:       "e2e 自动批准",
				})
				if err != nil {
					t.Fatalf("构造 approval.resolve: %v", err)
				}
				write(t, conn, resolve)
			case domain.EventToolResult:
				run.toolResults = append(run.toolResults, decodeEvent[domain.ToolResultPayload](t, p))
			case domain.EventProposalApplied:
				run.applied = append(run.applied, decodeEvent[domain.ProposalAppliedPayload](t, p))
			case domain.EventProposalRejected:
				run.rejected = append(run.rejected, decodeEvent[domain.ProposalRejectedPayload](t, p))
			case domain.EventAssistantMessage:
				run.assistant = append(run.assistant, decodeEvent[domain.AssistantMessagePayload](t, p).Text)
			case domain.EventTaskStatus:
				status := decodeEvent[domain.TaskStatusPayload](t, p)
				if p.TaskID == string(task.ID) && status.To.Terminal() {
					run.terminal = status
					return run
				}
			}
		}
	}
}

func requireCompleted(t *testing.T, run *realRun) {
	t.Helper()
	if run.terminal.To != domain.TaskCompleted || run.terminal.Error != "" {
		t.Fatalf("终态 = %q（error=%q）；事件：%v\n提案拒绝：%+v\n助手消息：%v",
			run.terminal.To, run.terminal.Error, run.kinds, run.rejected, run.assistant)
	}
}

// 只读路径：模型必须真的调用 read_file，并把文件内容带回来。
func TestRealProviderReadOnly(t *testing.T) {
	provider := realProviderConfig(t)
	ts, application := newLedgerServer(t)
	configureRealRuntime(t, application, provider)

	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("天空是蓝色的\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess, err := application.CreateSession(context.Background(), dir, "e2e", provider.Model, "真实只读")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	run := runRealTask(t, wsURL(ts), application, sess.ID,
		"请用 read_file 工具读取 notes.txt，然后把第一行原文一字不差地返回给我。不要凭猜测回答。")

	requireCompleted(t, run)
	res, ok := run.toolResult("read_file")
	if !ok {
		t.Fatalf("模型没有调用 read_file；工具结果：%+v", run.toolResults)
	}
	if res.IsError || !strings.Contains(res.Output, "天空是蓝色的") {
		t.Fatalf("read_file 结果 = %+v", res)
	}
	if run.count(domain.EventApprovalRequested) != 0 {
		t.Fatalf("只读路径不该弹审批，实际 %d 次", run.count(domain.EventApprovalRequested))
	}
}

// 写入路径：diff 审批 → 批准 → 提案落盘。
func TestRealProviderWriteApproval(t *testing.T) {
	provider := realProviderConfig(t)
	ts, application := newLedgerServer(t)
	configureRealRuntime(t, application, provider)

	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("旧内容\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess, err := application.CreateSession(context.Background(), dir, "e2e", provider.Model, "真实写入审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	run := runRealTask(t, wsURL(ts), application, sess.ID,
		"请用 write_file 工具把 notes.txt 的内容整体替换成这一行：你好，jiaoag。只做这一件事，不要用 run_command。")

	requireCompleted(t, run)
	if len(run.approvals) == 0 {
		t.Fatalf("写入必须经过审批；事件：%v", run.kinds)
	}
	// 模型可能先用命令（例如 cat）再看文件，第一条审批不一定是 diff；
	// 只要本次出现过 diff 审批，就说明写入路径确实走了提案。
	var diffSeen bool
	for _, ap := range run.approvals {
		if ap.Kind == domain.ApprovalDiff {
			diffSeen = true
			break
		}
	}
	if !diffSeen {
		t.Fatalf("写入必须走 diff 审批，实际：%+v", run.approvals)
	}
	// 批准后必须真的落盘：写入路径与目标文件都要对得上，只看「有 applied
	// 事件」会把「写错文件」当成通过。
	var applied bool
	for _, ap := range run.applied {
		if strings.HasSuffix(ap.Path, "notes.txt") {
			applied = true
			break
		}
	}
	if !applied {
		t.Fatalf("批准后应有 notes.txt 的 proposal.applied；实际：%+v", run.applied)
	}
	content, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "jiaoag") {
		t.Fatalf("落盘内容 = %q", content)
	}
}

// 命令路径：非白名单命令触发审批 → 批准 → 真实执行 → exit_code 0。
func TestRealProviderCommandApproval(t *testing.T) {
	provider := realProviderConfig(t)
	ts, application := newLedgerServer(t)
	configureRealRuntime(t, application, provider)

	dir := t.TempDir()
	sess, err := application.CreateSession(context.Background(), dir, "e2e", provider.Model, "真实命令审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	run := runRealTask(t, wsURL(ts), application, sess.ID,
		"请用 run_command 工具执行命令：echo jiaoag-e2e，然后把 stdout 原样返回。不要用 check_command 先检查，直接执行。")

	requireCompleted(t, run)
	var commandApproval bool
	for _, req := range run.approvals {
		if req.Kind == domain.ApprovalCommand {
			commandApproval = true
			if len(req.Command) == 0 || req.Cwd == "" || req.Backend == "" {
				t.Fatalf("命令审批载荷不完整：%+v", req)
			}
		}
	}
	if !commandApproval {
		t.Fatalf("非白名单命令必须触发命令审批；事件：%v", run.kinds)
	}
	var executed []domain.ToolResultPayload
	for _, res := range run.toolResults {
		if res.Tool == "run_command" {
			executed = append(executed, res)
		}
	}
	if len(executed) == 0 {
		t.Fatalf("模型没有调用 run_command；工具结果：%+v", run.toolResults)
	}
	// 模型可能先猜错参数、再重试一次：只要有一条成功且 stdout 对得上，
	// 就说明「审批 → 真实执行」这条链完整。
	okRun := false
	for _, res := range executed {
		if !res.IsError && res.ExitCode != nil && *res.ExitCode == 0 && strings.Contains(res.Output, "jiaoag-e2e") {
			okRun = true
			break
		}
	}
	if !okRun {
		t.Fatalf("run_command 没有一次成功执行；实际：%+v", executed)
	}
}
