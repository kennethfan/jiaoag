// Command jiaoag-demo 是人工验收用的最小客户端：连接真实服务端，建会话、
// 订阅事件、提交任务，遇到审批按指定策略裁决，并把事件时间线打到 stdout。
//
// 它不替代 internal/server 的 realprovider 测试：测试断言状态机，这里只负责
// 让人看见一轮真实任务长什么样（diff、命令、工具结果、token 用量）。
// 典型入口是 scripts/demo.sh（make demo）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func main() {
	var (
		url          = flag.String("url", "", "服务端 WS 地址，例如 ws://127.0.0.1:18787/ws")
		token        = flag.String("token", "", "server.token")
		workspace    = flag.String("workspace", "", "会话工作区（真实目录）")
		prompt       = flag.String("prompt", "", "任务内容")
		provider     = flag.String("provider", "", "供应商名（空表示用配置里的默认供应商）")
		model        = flag.String("model", "", "会话模型 id（空表示用供应商配置里的 model）")
		taskProvider = flag.String("task-provider", "", "只覆盖本次任务的供应商（空表示跟随会话）")
		taskModel    = flag.String("task-model", "", "只覆盖本次任务的模型（空表示跟随会话）")
		rootFlag     = flag.String("root", "", "只读附加根 alias=path（可选，ADR-0036）")
		decision     = flag.String("approve", "allow_once", "审批策略：allow_once | allow_session | deny")
		timeout      = flag.Duration("timeout", 5*time.Minute, "整轮超时")
	)
	flag.Parse()
	if *url == "" || *token == "" || *workspace == "" || *prompt == "" {
		flag.Usage()
		os.Exit(2)
	}
	switch *decision {
	case "allow_once", "allow_session", "deny":
	default:
		fmt.Fprintf(os.Stderr, "jiaoag-demo: 未知审批策略 %q\n", *decision)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	started := time.Now()
	conn, hello, err := dial(ctx, *url, *token)
	if err != nil {
		fail(err)
	}
	defer conn.CloseNow()
	fmt.Printf("已连接: server=%s protocol=v%d\n", hello.ServerVersion, hello.ProtocolVersion)

	var roots []protocol.SessionRootPayload
	if *rootFlag != "" {
		alias, path, ok := strings.Cut(*rootFlag, "=")
		if !ok || strings.TrimSpace(alias) == "" || strings.TrimSpace(path) == "" {
			fail(fmt.Errorf("-root 需要 alias=path 形式，收到 %q", *rootFlag))
		}
		roots = []protocol.SessionRootPayload{{Alias: strings.TrimSpace(alias), Path: strings.TrimSpace(path)}}
	}
	sessionID, err := createSession(ctx, conn, *workspace, *provider, *model, roots)
	if err != nil {
		fail(err)
	}
	fmt.Printf("会话: %s workspace=%s provider=%s model=%s\n", sessionID, *workspace, *provider, *model)

	if err := send(ctx, conn, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: sessionID, FromSeq: 0,
	}); err != nil {
		fail(err)
	}
	if err := send(ctx, conn, protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{
		SessionID: sessionID, Text: *prompt,
		Provider: *taskProvider, Model: *taskModel,
	}); err != nil {
		fail(err)
	}
	fmt.Printf("已提交任务: %s\n\n", *prompt)

	final, err := follow(ctx, conn, sessionID, *decision)
	if err != nil {
		fail(err)
	}
	fmt.Printf("\n任务终态: %s (round=%d tokens=%d/%d, 耗时 %s)\n",
		final.To, final.Round, final.PromptTok, final.OutputTok, time.Since(started).Round(time.Second))
	if final.To != domain.TaskCompleted {
		if final.Error != "" {
			fmt.Fprintf(os.Stderr, "失败原因: %s\n", final.Error)
		}
		os.Exit(1)
	}
}

func dial(ctx context.Context, url, token string) (*websocket.Conn, protocol.HelloPayload, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		return nil, protocol.HelloPayload{}, fmt.Errorf("连接 %s 失败: %w", url, err)
	}
	conn.SetReadLimit(4 << 20)

	var hello protocol.Envelope
	if err := wsjson.Read(ctx, conn, &hello); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("读 hello 失败: %w", err)
	}
	if hello.Type != protocol.TypeHello {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("期待 hello，收到 %s", hello.Type)
	}
	var hp protocol.HelloPayload
	if err := hello.Decode(&hp); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, err
	}
	if hp.ProtocolVersion != protocol.Version {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("协议版本不匹配：服务端 %d，客户端 %d", hp.ProtocolVersion, protocol.Version)
	}
	if err := send(ctx, conn, protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion: "jiaoag-demo", ProtocolVersion: protocol.Version,
	}); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, err
	}
	return conn, hp, nil
}

func createSession(ctx context.Context, conn *websocket.Conn, workspace, provider, model string, roots []protocol.SessionRootPayload) (string, error) {
	if err := send(ctx, conn, protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: workspace, Roots: roots, Provider: provider, Model: model,
	}); err != nil {
		return "", err
	}
	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return "", fmt.Errorf("等 session.created 失败: %w", err)
		}
		switch env.Type {
		case protocol.TypeSessionCreated:
			var sp protocol.SessionPayload
			if err := env.Decode(&sp); err != nil {
				return "", err
			}
			return sp.ID, nil
		case protocol.TypeError:
			return "", serverError(env)
		}
	}
}

// follow 打印事件直到任务终态，返回最后一条 task.status 的载荷。
func follow(ctx context.Context, conn *websocket.Conn, sessionID, decision string) (domain.TaskStatusPayload, error) {
	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return domain.TaskStatusPayload{}, fmt.Errorf("读事件失败: %w", err)
		}
		if env.Type == protocol.TypeError {
			return domain.TaskStatusPayload{}, serverError(env)
		}
		if env.Type != protocol.TypeEvent {
			continue
		}
		var se protocol.StreamEventPayload
		if err := env.Decode(&se); err != nil {
			continue
		}
		kind := domain.EventKind(se.Kind)
		if kind == domain.EventAssistantDelta {
			continue // 增量不落库也不值得刷屏，完整消息在 assistant.message
		}
		if kind == domain.EventApprovalRequested {
			var p domain.ApprovalRequestedPayload
			_ = decode(se.Payload, &p)
			showApproval(env.Seq, p)
			if err := send(ctx, conn, protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
				SessionID: sessionID, ApprovalID: string(p.ApprovalID), Decision: decision,
			}); err != nil {
				return domain.TaskStatusPayload{}, err
			}
			fmt.Printf("[%3d] approval.resolve   → %s\n", env.Seq, decision)
			continue
		}
		if kind == domain.EventTaskStatus {
			var p domain.TaskStatusPayload
			_ = decode(se.Payload, &p)
			fmt.Printf("[%3d] task.status        %s → %s %s\n", env.Seq, p.From, p.To, p.Reason)
			if p.To.Terminal() {
				return p, nil
			}
			continue
		}
		fmt.Printf("[%3d] %-20s %s\n", env.Seq, kind, summarize(kind, se.Payload))
	}
}

func showApproval(seq int64, p domain.ApprovalRequestedPayload) {
	fmt.Printf("[%3d] approval.requested kind=%s risk=%s path=%s\n", seq, p.Kind, p.Risk, p.Path)
	if len(p.Command) > 0 {
		fmt.Printf("      command=%v cwd=%s backend=%s timeout=%dms\n", p.Command, p.Cwd, p.Backend, p.TimeoutMS)
	}
	if p.Diff != "" {
		for _, line := range strings.Split(strings.TrimRight(p.Diff, "\n"), "\n") {
			fmt.Printf("      | %s\n", line)
		}
	}
}

func summarize(kind domain.EventKind, raw json.RawMessage) string {
	switch kind {
	case domain.EventAssistantMessage:
		var p domain.AssistantMessagePayload
		if decode(raw, &p) == nil {
			text := oneLine(p.Text)
			if len(p.ToolCalls) == 0 {
				return text
			}
			names := make([]string, 0, len(p.ToolCalls))
			for _, tc := range p.ToolCalls {
				names = append(names, tc.Name+" "+string(tc.Args))
			}
			return strings.TrimSpace(text + " → " + strings.Join(names, "; "))
		}
	case domain.EventToolRequested:
		var p domain.ToolRequestedPayload
		if decode(raw, &p) == nil {
			return p.Tool + " " + oneLine(string(p.Args))
		}
	case domain.EventToolResult:
		var p domain.ToolResultPayload
		if decode(raw, &p) == nil {
			return fmt.Sprintf("%s status=%s exit=%s %s", p.Tool, p.Status, exitText(p.ExitCode), oneLine(p.Output))
		}
	case domain.EventProposalApplied:
		var p domain.ProposalAppliedPayload
		if decode(raw, &p) == nil {
			return "已落盘 " + p.Path
		}
	case domain.EventRoundStarted:
		var p domain.RoundStartedPayload
		if decode(raw, &p) == nil {
			return fmt.Sprintf("round %d", p.Round)
		}
	}
	return oneLine(string(raw))
}

func exitText(code *int) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprint(*code)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	const limit = 220
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

func send(ctx context.Context, conn *websocket.Conn, typ string, payload any) error {
	env, err := protocol.New(typ, payload)
	if err != nil {
		return fmt.Errorf("构造 %s 失败: %w", typ, err)
	}
	if err := wsjson.Write(ctx, conn, env); err != nil {
		return fmt.Errorf("发送 %s 失败: %w", typ, err)
	}
	return nil
}

func serverError(env protocol.Envelope) error {
	var p protocol.ErrorPayload
	_ = env.Decode(&p)
	return fmt.Errorf("服务端错误 %s: %s", p.Code, p.Message)
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("空载荷")
	}
	return json.Unmarshal(raw, v)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag-demo: %v\n", err)
	os.Exit(1)
}
