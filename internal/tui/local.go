package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
)

// localTransport 是 TUI 内嵌运行时的帧通道：不经过端口、WebSocket 或令牌，
// 直接把帧投影成契约调用，再把结果投影回协议载荷。
//
// 它并不「重新实现服务端」：类型常量、载荷与错误码都来自 internal/protocol，
// 业务逻辑在 internal/rpc，WebSocket 只是这套契约的另一层适配器。
type localTransport struct {
	svc     rpc.Service
	streams *rpc.Streams
	pr      rpc.Principal

	in     chan protocol.Envelope
	closed chan struct{}
	once   sync.Once

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewLocalTransport 构造进程内的 Transport。principal 决定读写权限：
// TUI 自己拥有运行时，用 rpc.Owner()（可写）。
func NewLocalTransport(svc rpc.Service, streams *rpc.Streams, pr rpc.Principal) Transport {
	return &localTransport{
		svc:     svc,
		streams: streams,
		pr:      pr,
		in:      make(chan protocol.Envelope, 256),
		closed:  make(chan struct{}),
	}
}

func (t *localTransport) Read() (protocol.Envelope, error) {
	select {
	case env := <-t.in:
		return env, nil
	case <-t.closed:
		return protocol.Envelope{}, fmt.Errorf("连接已关闭")
	}
}

func (t *localTransport) Close() error {
	t.once.Do(func() {
		t.mu.Lock()
		if t.cancel != nil {
			t.cancel()
			t.cancel = nil
		}
		t.mu.Unlock()
		close(t.closed)
	})
	return nil
}

// Send 把一帧翻译成一次契约调用。没有回执的命令（approval.resolve、
// event.unsubscribe）与服务端一样静默；其余都回一帧，形状与 WS 完全一致。
func (t *localTransport) Send(env protocol.Envelope) error {
	ctx := context.Background()
	switch env.Type {
	case protocol.TypeClientHello, protocol.TypeEventUnsubscribe:
		if env.Type == protocol.TypeEventUnsubscribe {
			t.unsubscribe()
		}
		return nil
	case protocol.TypePing:
		return t.reply(protocol.TypePong, nil, "")
	case protocol.TypeEventSubscribe:
		return t.subscribe(ctx, env)
	}

	var err error
	switch env.Type {
	case protocol.TypeSessionCreate:
		var p protocol.SessionCreatePayload
		if err = env.Decode(&p); err == nil {
			var out protocol.SessionPayload
			out, err = t.svc.SessionCreate(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeSessionCreated, out, out.ID)
		}
	case protocol.TypeSessionUpdate:
		var p protocol.SessionUpdatePayload
		if err = env.Decode(&p); err == nil {
			var out protocol.SessionPayload
			out, err = t.svc.SessionUpdate(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeSessionUpdated, out, out.ID)
		}
	case protocol.TypeSessionManage:
		var p protocol.SessionManagePayload
		if err = env.Decode(&p); err == nil {
			var out protocol.SessionManageResultPayload
			out, err = t.svc.SessionManage(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeSessionManageResult, out, p.SessionID)
		}
	case protocol.TypeSessionRename:
		var p protocol.SessionRenamePayload
		if err = env.Decode(&p); err == nil {
			var out protocol.SessionPayload
			out, err = t.svc.SessionRename(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeSessionRenamed, out, out.ID)
		}
	case protocol.TypeSessionDelete:
		var p protocol.SessionDeletePayload
		if err = env.Decode(&p); err == nil {
			err = t.svc.SessionDelete(ctx, t.pr, p)
			if err == nil {
				err = t.reply(protocol.TypeSessionDeleted, protocol.SessionDeletedPayload(p), p.SessionID)
			}
		}
	case protocol.TypeSessionList:
		var out protocol.SessionListResultPayload
		out, err = t.svc.SessionList(ctx, t.pr)
		err = t.finish(err, protocol.TypeSessionListResult, out, "")
	case protocol.TypeTaskSubmit:
		var p protocol.TaskSubmitPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.TaskPayload
			out, err = t.svc.TaskSubmit(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeTaskQueued, out, p.SessionID)
		}
	case protocol.TypeTaskCancel:
		var p protocol.TaskCancelPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.TaskPayload
			out, err = t.svc.TaskCancel(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeTaskCanceled, out, p.SessionID)
		}
	case protocol.TypeApprovalResolve:
		var p protocol.ApprovalResolvePayload
		if err = env.Decode(&p); err == nil {
			err = t.svc.ApprovalResolve(ctx, t.pr, p)
		}
	case protocol.TypeProviderModels:
		var p protocol.ProviderModelsPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.ProviderModelsResultPayload
			out, err = t.svc.ProviderModels(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeProviderModelsResult, out, p.SessionID)
		}
	case protocol.TypeSkillList:
		var p protocol.SkillListPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.SkillListResultPayload
			out, err = t.svc.SkillList(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeSkillListResult, out, p.SessionID)
		}
	case protocol.TypeMCPList:
		var p protocol.MCPListPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.MCPListResultPayload
			out, err = t.svc.MCPList(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeMCPListResult, out, p.SessionID)
		}
	case protocol.TypeProjectTrust, protocol.TypeProjectRevoke:
		var p protocol.ProjectActionPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.ProjectStatusPayload
			out, err = t.svc.ProjectTrust(ctx, t.pr, p)
			if env.Type == protocol.TypeProjectRevoke {
				out, err = t.svc.ProjectRevoke(ctx, t.pr, p)
			}
			err = t.finish(err, protocol.TypeProjectTrusted, out, p.SessionID)
		}
	case protocol.TypeEventHead:
		var p protocol.EventHeadPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.EventHeadResultPayload
			out, err = t.svc.EventHead(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeEventHeadResult, out, p.SessionID)
		}
	case protocol.TypeAuditTail:
		var p protocol.AuditTailPayload
		if err = env.Decode(&p); err == nil {
			var out protocol.AuditTailResultPayload
			out, err = t.svc.AuditTail(ctx, t.pr, p)
			err = t.finish(err, protocol.TypeAuditTailResult, out, "")
		}
	default:
		err = &rpc.Error{Code: protocol.CodeUnsupported, Message: "不支持的消息类型: " + env.Type}
	}
	if err != nil {
		return t.pushError(err)
	}
	return nil
}

// finish 在成功时发回执、失败时把错误留给调用方统一转成 error 帧。
func (t *localTransport) finish(err error, typ string, payload any, sessionID string) error {
	if err != nil {
		return err
	}
	return t.reply(typ, payload, sessionID)
}

func (t *localTransport) reply(typ string, payload any, sessionID string) error {
	env, err := protocol.New(typ, payload)
	if err != nil {
		return err
	}
	env.SessionID = sessionID
	env.TS = time.Now().UnixMilli()
	return t.push(env)
}

func (t *localTransport) pushError(err error) error {
	env, nerr := protocol.New(protocol.TypeError, protocol.ErrorPayload{
		Code:    rpc.CodeOf(err),
		Message: err.Error(),
	})
	if nerr != nil {
		return nerr
	}
	return t.push(env)
}

func (t *localTransport) push(env protocol.Envelope) error {
	select {
	case t.in <- env:
		return nil
	case <-t.closed:
		return fmt.Errorf("连接已关闭")
	case <-time.After(5 * time.Second):
		return fmt.Errorf("本地帧缓冲已满")
	}
}

func (t *localTransport) subscribe(ctx context.Context, env protocol.Envelope) error {
	var p protocol.EventSubscribePayload
	if err := env.Decode(&p); err != nil {
		return t.pushError(err)
	}
	subCtx, cancel := context.WithCancel(ctx)
	ch, subCancel, err := t.streams.Subscribe(subCtx, p)
	if err != nil {
		cancel()
		return t.pushError(err)
	}
	t.unsubscribe()
	stop := func() {
		subCancel()
		cancel()
	}
	t.mu.Lock()
	t.cancel = stop
	t.mu.Unlock()

	go func() {
		defer func() {
			t.mu.Lock()
			if t.cancel != nil {
				t.cancel = nil
			}
			t.mu.Unlock()
		}()
		for {
			select {
			case <-subCtx.Done():
				return
			case <-t.closed:
				return
			case frame, ok := <-ch:
				if !ok {
					return
				}
				if err := t.push(frame); err != nil {
					return
				}
			}
		}
	}()
	return nil
}

func (t *localTransport) unsubscribe() {
	t.mu.Lock()
	cancel := t.cancel
	t.cancel = nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
