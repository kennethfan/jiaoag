package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// writeOnceProvider 第一轮让模型调 write_file，之后收尾。
//
// 不用真实供应商：审批接缝只关心「工具想写什么、裁决怎么回传」。
type writeOnceProvider struct{ wrote bool }

func (p *writeOnceProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试只走流式")
}

func (p *writeOnceProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if p.wrote {
		if err := fn(llm.Chunk{TextDelta: "写好了"}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd})
	}
	p.wrote = true
	if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{
		Index: 0, ID: "call_ws", Name: "write_file",
	}}); err != nil {
		return err
	}
	if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{
		Index: 0, ArgsFragment: `{"path":"out.txt","content":"hello from ws"}`,
	}}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopToolUse})
}

func configureWriteRuntime(t *testing.T, a *app.App, provider llm.Provider) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(
				tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
				tools.NewWriteFile(ws), tools.NewEditFile(ws),
			)
		},
		MaxRounds:     4,
		ContextBudget: 10000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// awaitEvent 读到第一条指定类型的 domain 事件；中间的临时帧与别的事件跳过。
func awaitEvent(t *testing.T, conn *websocket.Conn, kind domain.EventKind) protocol.StreamEventPayload {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		env := readEnvelope(t, conn)
		// 服务端送来的 error 帧（订阅被拒、会话不存在等）必须直接报出来：
		// 静默跳过只会让人在「等不到事件」的超时里找错方向。
		if env.Type == protocol.TypeError {
			var e protocol.ErrorPayload
			if err := env.Decode(&e); err != nil {
				t.Fatalf("解码 error 帧: %v", err)
			}
			t.Fatalf("服务端返回错误 %s：%s", e.Code, e.Message)
		}
		if env.Type != protocol.TypeEvent {
			continue
		}
		var p protocol.StreamEventPayload
		if err := env.Decode(&p); err != nil {
			t.Fatalf("解码事件帧: %v", err)
		}
		if p.Kind == string(kind) {
			return p
		}
	}
	t.Fatalf("等不到事件 %s", kind)
	return protocol.StreamEventPayload{}
}

func decodeEvent[T any](t *testing.T, p protocol.StreamEventPayload) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(p.Payload, &out); err != nil {
		t.Fatalf("解码 %s 载荷: %v", p.Kind, err)
	}
	return out
}

// commandOnceProvider 第一轮让模型调 run_command，之后收尾。
type commandOnceProvider struct {
	argv []string
	sent bool
}

func (p *commandOnceProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试只走流式")
}

func (p *commandOnceProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if p.sent {
		if err := fn(llm.Chunk{TextDelta: "没有删除"}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd})
	}
	p.sent = true
	args, err := json.Marshal(map[string]any{"argv": p.argv})
	if err != nil {
		return err
	}
	if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{
		Index: 0, ID: "call_rm", Name: "run_command",
	}}); err != nil {
		return err
	}
	if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{
		Index: 0, ArgsFragment: string(args),
	}}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopToolUse})
}

// configureCommandRunner 接上真实执行后端与默认规则集，供命令审批接缝使用。
func configureCommandRunner(t *testing.T, a *app.App, provider llm.Provider) {
	t.Helper()
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
			)
		},
		Backend:       execpkg.NewDirect(),
		Ruleset:       perm.DefaultRuleset(),
		OutputDir:     filepath.Join(t.TempDir(), "outputs"),
		ExecTimeout:   30 * time.Second,
		MaxRounds:     4,
		ContextBudget: 20000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// 命令审批接缝 B：approval.requested{kind:command} 经 WS 到达客户端，拒绝后
// 事件回环、工具结果标 denied，且进程一个字节都没有启动。
func TestCommandApprovalOverWS(t *testing.T) {
	ts, application := newLedgerServer(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	configureCommandRunner(t, application, &commandOnceProvider{argv: []string{"rm", "-rf", "marker.txt"}})

	ctx := context.Background()
	sess, err := application.CreateSession(ctx, dir, "openai", "m", "命令审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := application.SubmitTask(ctx, sess.ID, "删掉 marker.txt")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)

	req := decodeEvent[domain.ApprovalRequestedPayload](t,
		awaitEvent(t, conn, domain.EventApprovalRequested))
	if req.Kind != domain.ApprovalCommand {
		t.Fatalf("审批 kind = %q，期望 command", req.Kind)
	}
	if len(req.Command) != 3 || req.Command[0] != "rm" {
		t.Fatalf("审批 argv = %v", req.Command)
	}
	if req.Cwd == "" || req.Backend != "direct" || req.TimeoutMS <= 0 {
		t.Fatalf("审批载荷缺少 cwd/backend/timeout：%+v", req)
	}

	resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
		SessionID:  string(sess.ID),
		ApprovalID: string(req.ApprovalID),
		Decision:   string(domain.DecisionDeny),
		Note:       "不要删",
	})
	if err != nil {
		t.Fatalf("构造 approval.resolve: %v", err)
	}
	write(t, conn, resolve)

	resolved := decodeEvent[domain.ApprovalResolvedPayload](t,
		awaitEvent(t, conn, domain.EventApprovalResolved))
	if resolved.Decision != domain.DecisionDeny || resolved.Source != domain.SourceUser {
		t.Fatalf("裁决 = %s/%s", resolved.Decision, resolved.Source)
	}

	result := decodeEvent[domain.ToolResultPayload](t,
		awaitEvent(t, conn, domain.EventToolResult))
	if result.Tool != "run_command" || result.Status != string(domain.ToolDenied) || !result.IsError {
		t.Fatalf("tool.result = %+v，期望 run_command/denied", result)
	}

	// 拒绝必须意味着「一个字节都没执行」：文件还在就是最直接的证据。
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("拒绝后命令不应被执行：%v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		done, err := application.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if done.Status.Terminal() {
			if done.Status != domain.TaskCompleted {
				t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("任务没有进入终态")
}

// 审批接缝 A：approval.requested 经事件帧到达客户端，裁决后 approval.resolved
// 与 proposal.applied 回到客户端，落盘内容与任务终态都符合预期。
func TestApprovalOverWS(t *testing.T) {
	ts, application := newLedgerServer(t)
	dir := t.TempDir()
	configureWriteRuntime(t, application, &writeOnceProvider{})

	ctx := context.Background()
	sess, err := application.CreateSession(ctx, dir, "openai", "m", "审批")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := application.SubmitTask(ctx, sess.ID, "写个文件")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	// 从 0 开始订阅：订阅前的提案与审批由回放补齐，不依赖计时。
	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)

	requested := awaitEvent(t, conn, domain.EventApprovalRequested)
	req := decodeEvent[domain.ApprovalRequestedPayload](t, requested)
	if req.Path != "out.txt" {
		t.Fatalf("审批路径 = %q", req.Path)
	}
	if !strings.Contains(req.Diff, "+hello from ws") {
		t.Fatalf("审批载荷里的 diff 不对：\n%s", req.Diff)
	}

	resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
		SessionID:  string(sess.ID),
		ApprovalID: string(req.ApprovalID),
		Decision:   string(domain.DecisionAllowOnce),
	})
	if err != nil {
		t.Fatalf("构造 approval.resolve: %v", err)
	}
	write(t, conn, resolve)

	resolved := decodeEvent[domain.ApprovalResolvedPayload](t,
		awaitEvent(t, conn, domain.EventApprovalResolved))
	if resolved.ApprovalID != req.ApprovalID {
		t.Fatalf("裁决的审批 = %s，期望 %s", resolved.ApprovalID, req.ApprovalID)
	}
	if resolved.Decision != domain.DecisionAllowOnce || resolved.Source != domain.SourceUser {
		t.Fatalf("裁决 = %s/%s", resolved.Decision, resolved.Source)
	}
	awaitEvent(t, conn, domain.EventProposalApplied)

	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || string(got) != "hello from ws" {
		t.Fatalf("落盘内容 = %q，err = %v", got, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		done, err := application.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if done.Status.Terminal() {
			if done.Status != domain.TaskCompleted {
				t.Fatalf("终态 = %s（%s）", done.Status, done.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("任务没有进入终态")
}

// 未知裁决是客户端错误：必须回 bad_request，而不是服务端内部故障。
func TestApprovalResolveUnknownDecisionIsBadRequest(t *testing.T) {
	ts, application := newLedgerServer(t)
	ctx := context.Background()
	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "m", "校验")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := application.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approval := &domain.Approval{
		ID: "apr_1", TaskID: task.ID, ToolCallID: "call_x",
		Kind: domain.ApprovalDiff, Risk: domain.RiskMedium, RequestedAt: domain.Now(),
	}
	if err := application.Record(ctx, domain.Transition{
		TaskID: task.ID, Approval: approval,
	}); err != nil {
		t.Fatalf("写入审批: %v", err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
		SessionID: string(sess.ID), ApprovalID: "apr_1", Decision: "yolo",
	})
	if err != nil {
		t.Fatalf("构造 approval.resolve: %v", err)
	}
	write(t, conn, resolve)

	env := readEnvelope(t, conn)
	if env.Type != protocol.TypeError {
		t.Fatalf("类型 = %s，期望 error", env.Type)
	}
	var payload protocol.ErrorPayload
	if err := env.Decode(&payload); err != nil {
		t.Fatalf("解码 error: %v", err)
	}
	if payload.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s，期望 %s", payload.Code, protocol.CodeBadRequest)
	}
}

// 重复 resolve 是幂等空操作：两次都不回 error 帧，连接仍然可用。
func TestApprovalResolveRepeatIsSilent(t *testing.T) {
	ts, application := newLedgerServer(t)
	ctx := context.Background()
	sess, err := application.CreateSession(ctx, t.TempDir(), "openai", "m", "幂等")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := application.SubmitTask(ctx, sess.ID, "占位")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	approval := &domain.Approval{
		ID: "apr_2", TaskID: task.ID, ToolCallID: "call_x",
		Kind: domain.ApprovalDiff, Risk: domain.RiskMedium, RequestedAt: domain.Now(),
	}
	if err := application.Record(ctx, domain.Transition{
		TaskID: task.ID, Approval: approval,
	}); err != nil {
		t.Fatalf("写入审批: %v", err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)

	for i := 0; i < 2; i++ {
		resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
			SessionID: string(sess.ID), ApprovalID: "apr_2",
			Decision: string(domain.DecisionAllowOnce),
		})
		if err != nil {
			t.Fatalf("构造 approval.resolve: %v", err)
		}
		write(t, conn, resolve)
	}

	// 裁决不产生回执帧；用 ping/pong 确认连接仍然健康，反证上面没有送回 error。
	frames := frameStream(conn)
	select {
	case env := <-frames:
		t.Fatalf("裁决不该回帧，收到 %s", env.Type)
	case <-time.After(500 * time.Millisecond):
	}
	ping, err := protocol.New(protocol.TypePing, nil)
	if err != nil {
		t.Fatalf("构造 ping: %v", err)
	}
	write(t, conn, ping)
	select {
	case env := <-frames:
		if env.Type != protocol.TypePong {
			t.Fatalf("类型 = %s，期望 pong", env.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等不到 pong")
	}
}

// frameStream 把连接上的帧持续读进 channel。
//
// 不能改用「带超时的 Read」来断言「没有帧」：coder/websocket 在读上下文被
// 取消后会直接关闭连接，后面就什么都测不了了。
func frameStream(conn *websocket.Conn) <-chan protocol.Envelope {
	ch := make(chan protocol.Envelope, 16)
	go func() {
		defer close(ch)
		for {
			var env protocol.Envelope
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := wsjson.Read(ctx, conn, &env)
			cancel()
			if err != nil {
				return
			}
			ch <- env
		}
	}()
	return ch
}
