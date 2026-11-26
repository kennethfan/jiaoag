package rpc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// replayBatch 是单次回放的分批大小。存储层允许 1..1000，这里取 500 作为
// 折中：首屏不会一次拉太多，积压较多时往返次数也可控。
const replayBatch = 500

// streamBuffer 是本地订阅通道的容量。远程 WS 的积压由服务器连接的写队列
// 吸收；进程内没有网络缓冲，必须自己给一个上限，否则慢消费者会把内存吃光。
const streamBuffer = 256

// Streams 把「订阅 → 回放 → 快照 → 去重 → 实时」这条链路封成一份实现，
// 输出统一的协议信封。WS 适配器把它写进连接，TUI 直接读这个通道。
type Streams struct {
	app *app.App
	log *slog.Logger
}

func NewStreams(a *app.App, log *slog.Logger) *Streams {
	return &Streams{app: a, log: log}
}

// Subscribe 返回一个只读通道与取消函数。通道关闭表示订阅结束；
// 积压溢出时通道里会先出现一条错误帧再关闭。
func (s *Streams) Subscribe(ctx context.Context, in protocol.EventSubscribePayload) (<-chan protocol.Envelope, func(), error) {
	if s.app == nil {
		return nil, nil, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	sessionID := domain.SessionID(in.SessionID)
	if !sessionID.Valid() {
		return nil, nil, badRequest("event.subscribe 缺少 session_id")
	}
	if _, err := s.app.GetSession(ctx, sessionID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, notFound(err)
		}
		return nil, nil, internal(err)
	}
	if in.FromSeq < 0 {
		return nil, nil, badRequest("from_seq 不能为负数")
	}

	subCtx, cancel := context.WithCancel(ctx)
	sub := s.app.Hub().Subscribe(sessionID)
	out := make(chan protocol.Envelope, streamBuffer)
	go s.run(subCtx, out, sub, sessionID, domain.Seq(in.FromSeq))
	return out, cancel, nil
}

func (s *Streams) run(ctx context.Context, out chan<- protocol.Envelope, sub *event.Subscription,
	sessionID domain.SessionID, from domain.Seq) {
	defer sub.Close()
	defer close(out)

	sent := from
	// 1) 回放：把 from 之后的既有事件补齐。
	for {
		batch, err := s.app.Replay(ctx, sessionID, sent, replayBatch)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.pushError(ctx, out, protocol.CodeInternal, err.Error())
			return
		}
		for _, ev := range batch {
			if !pushEvent(ctx, out, ev) {
				return
			}
			sent = ev.Seq
		}
		if len(batch) < replayBatch {
			break
		}
	}

	// 2) 增量订阅补快照：approval.requested 如果发生在 from 之前，回放永远
	// 不会补到它。全量回放（from=0）本身就包含待审批，不需要快照。
	if from > 0 {
		if err := s.pushSnapshot(ctx, out, sessionID, from); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.pushError(ctx, out, protocol.CodeInternal, err.Error())
			return
		}
	}

	// 3) 清空回放期间进入缓冲的事件；已发送过的 seq 直接丢弃。
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				s.reportOverflow(ctx, out, sub)
				return
			}
			if ev.Seq != 0 && ev.Seq <= sent {
				continue
			}
			if !pushEvent(ctx, out, ev) {
				return
			}
			if ev.Seq != 0 {
				sent = ev.Seq
			}
		default:
			goto live
		}
	}

live:
	// 4) 实时推送。缓冲写满意味着消费者跟不上：明确告知，让它带 from_seq 重连。
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Overflow():
			s.reportOverflow(ctx, out, sub)
			return
		case ev, ok := <-sub.C():
			if !ok {
				s.reportOverflow(ctx, out, sub)
				return
			}
			if ev.Seq != 0 && ev.Seq <= sent {
				continue
			}
			if !pushEvent(ctx, out, ev) {
				return
			}
			if ev.Seq != 0 {
				sent = ev.Seq
			}
		}
	}
}

func (s *Streams) pushSnapshot(ctx context.Context, out chan<- protocol.Envelope, sessionID domain.SessionID, from domain.Seq) error {
	head, err := s.app.LastSeq(ctx, sessionID)
	if err != nil {
		return err
	}
	payload := protocol.EventSnapshotPayload{HeadSeq: int64(head)}
	if int64(from) > int64(head) {
		payload.Reset = true
	} else {
		raws, err := s.app.PendingApprovalPayloads(ctx, sessionID)
		if err != nil {
			return err
		}
		payload.Pending = make([]protocol.StreamEventPayload, 0, len(raws))
		for _, raw := range raws {
			payload.Pending = append(payload.Pending, protocol.StreamEventPayload{
				Kind:    string(domain.EventApprovalRequested),
				Payload: raw,
			})
		}
	}
	if !payload.Reset && len(payload.Pending) == 0 {
		return nil
	}
	env, err := protocol.New(protocol.TypeEventSnapshot, payload)
	if err != nil {
		return err
	}
	env.SessionID = string(sessionID)
	if !send(ctx, out, env) {
		return ctx.Err()
	}
	return nil
}

func (s *Streams) reportOverflow(ctx context.Context, out chan<- protocol.Envelope, sub *event.Subscription) {
	select {
	case <-sub.Overflow():
		s.pushError(ctx, out, protocol.CodeInternal, "事件积压超出缓冲，请带 from_seq 重连")
	default:
	}
}

func (s *Streams) pushError(ctx context.Context, out chan<- protocol.Envelope, code, message string) {
	env, err := protocol.New(protocol.TypeError, protocol.ErrorPayload{Code: code, Message: message})
	if err != nil {
		return
	}
	_ = send(ctx, out, env)
}

// send 在 ctx 结束或消费者长时间不读取时放弃，避免把订阅协程卡死。
func send(ctx context.Context, out chan<- protocol.Envelope, env protocol.Envelope) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- env:
		return true
	}
}

func pushEvent(ctx context.Context, out chan<- protocol.Envelope, ev domain.Event) bool {
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    string(ev.Type),
		TaskID:  string(ev.TaskID),
		Payload: ev.Payload,
	})
	if err != nil {
		return true // 单条坏事件不该终止整条订阅
	}
	env.SessionID = string(ev.SessionID)
	env.Seq = int64(ev.Seq)
	env.TS = ev.At.UnixMilli()
	return send(ctx, out, env)
}
