package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// replayBatch 是单次回放的分批大小。存储层允许 1..1000，这里取 500 作为
// 折中：首屏不会一次拉太多，积压较多时往返次数也可控。
const replayBatch = 500

func (s *Server) handleSessionCreate(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.SessionCreatePayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	sess, err := s.app.CreateSession(ctx, p.Workspace, p.Provider, p.Model, p.Title)
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	reply, err := protocol.New(protocol.TypeSessionCreated, sessionPayload(sess))
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = string(sess.ID)
	reply.TS = sess.CreatedAt.UnixMilli()
	_ = c.send(ctx, reply)
}

func (s *Server) handleSessionList(ctx context.Context, c *clientConn) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	sessions, err := s.app.ListSessions(ctx)
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	out := make([]protocol.SessionPayload, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionPayload(sess))
	}
	reply, err := protocol.New(protocol.TypeSessionListResult, protocol.SessionListResultPayload{Sessions: out})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	_ = c.send(ctx, reply)
}

func (s *Server) handleTaskSubmit(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.TaskSubmitPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	task, err := s.app.SubmitTask(ctx, domain.SessionID(p.SessionID), p.Text)
	if err != nil {
		// 会话忙是可恢复的客户端错误，单独给码，便于界面提示「先取消」。
		code := protocol.CodeBadRequest
		if errors.Is(err, app.ErrSessionBusy) {
			code = protocol.CodeSessionBusy
		}
		c.sendError(ctx, code, err.Error())
		return
	}
	reply, err := protocol.New(protocol.TypeTaskQueued, taskPayload(task))
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = string(task.SessionID)
	reply.TS = task.CreatedAt.UnixMilli()
	_ = c.send(ctx, reply)
}

// handleTaskCancel 取消排队中或运行中的任务。
//
// 运行中的任务不在本调用里写终态：agent 观察 ctx 结束后自己记 canceled，
// 这样「谁在什么时候把任务停掉的」只有一条写入路径。
func (s *Server) handleTaskCancel(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.TaskCancelPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	sessionID := domain.SessionID(p.SessionID)
	taskID := domain.TaskID(p.TaskID)
	if !sessionID.Valid() || !taskID.Valid() {
		c.sendError(ctx, protocol.CodeBadRequest, "task.cancel 需要 session_id 与 task_id")
		return
	}
	if err := s.app.CancelTask(ctx, sessionID, taskID); err != nil {
		code := protocol.CodeInternal
		if errors.Is(err, domain.ErrNotFound) {
			code = protocol.CodeBadRequest
		}
		c.sendError(ctx, code, err.Error())
		return
	}
	reply, err := protocol.New(protocol.TypeTaskCanceled, protocol.TaskPayload{
		ID:        p.TaskID,
		SessionID: p.SessionID,
		Status:    string(domain.TaskCanceled),
	})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = p.SessionID
	_ = c.send(ctx, reply)
}

// handleApprovalResolve 把用户的裁决交给正在等待的 agent。
//
// 不回执帧：裁决的结果由 agent 写成 approval.resolved 事件，客户端从事件流
// 里看到它。重复裁决是幂等的空操作，已过期的审批也直接成功返回——用户点两下
// 不该收到一个「错误」。
func (s *Server) handleApprovalResolve(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.ApprovalResolvePayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	if p.SessionID == "" || p.ApprovalID == "" {
		c.sendError(ctx, protocol.CodeBadRequest, "approval.resolve 需要 session_id 与 approval_id")
		return
	}
	// 这里按线协议校验一次：未知裁决是客户端错误，不该报成服务端内部故障。
	switch domain.Decision(p.Decision) {
	case domain.DecisionAllowOnce, domain.DecisionAllowSession, domain.DecisionDeny:
	default:
		c.sendError(ctx, protocol.CodeBadRequest,
			"approval.resolve 的 decision 必须是 allow_once | allow_session | deny")
		return
	}
	if err := s.app.ResolveApproval(ctx, domain.SessionID(p.SessionID),
		domain.ApprovalID(p.ApprovalID), domain.Decision(p.Decision), p.Note); err != nil {
		code := protocol.CodeInternal
		if errors.Is(err, domain.ErrNotFound) {
			code = protocol.CodeBadRequest
		}
		c.sendError(ctx, code, err.Error())
	}
}

// handleEventSubscribe 启动一条独立的写协程：订阅 → 回放 → 刷缓冲去重 → 实时推送。
//
// 顺序不能反：先订阅才能保证回放期间产生的事件不会漏；回放之后必须清空
// 缓冲并把已发过的 seq 丢掉，否则会重复。
func (s *Server) handleEventSubscribe(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.EventSubscribePayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	sessionID := domain.SessionID(p.SessionID)
	if !sessionID.Valid() {
		c.sendError(ctx, protocol.CodeBadRequest, "event.subscribe 缺少 session_id")
		return
	}
	if _, err := s.app.GetSession(ctx, sessionID); err != nil {
		code := protocol.CodeInternal
		if errors.Is(err, domain.ErrNotFound) {
			code = protocol.CodeBadRequest
		}
		c.sendError(ctx, code, err.Error())
		return
	}
	if p.FromSeq < 0 {
		c.sendError(ctx, protocol.CodeBadRequest, "from_seq 不能为负数")
		return
	}

	// 同一连接上重复订阅同一会话时，先取消旧的推送协程再起新的：
	// 协议承诺「不重复、不乱序」，两份订阅会让每条事件发送两次。
	key := string(sessionID)
	subCtx, cancel := context.WithCancel(ctx)
	handle := c.beginStream(key, cancel)
	sub := s.app.Hub().Subscribe(sessionID)
	go func() {
		defer c.endStream(key, handle)
		s.streamEvents(subCtx, c, sub, sessionID, domain.Seq(p.FromSeq))
	}()
}

func (s *Server) streamEvents(ctx context.Context, c *clientConn, sub *event.Subscription, sessionID domain.SessionID, from domain.Seq) {
	defer sub.Close()

	sent := from
	// 1) 回放：把 from 之后的既有事件补齐。
	for {
		batch, err := s.app.Replay(ctx, sessionID, sent, replayBatch)
		if err != nil {
			c.sendError(ctx, protocol.CodeInternal, err.Error())
			return
		}
		for _, ev := range batch {
			if err := sendEvent(ctx, c, ev); err != nil {
				return
			}
			sent = ev.Seq
		}
		if len(batch) < replayBatch {
			break
		}
	}

	// 2) 清空回放期间进入缓冲的事件；已发送过的 seq 直接丢弃。
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				s.reportOverflow(ctx, c, sub)
				return
			}
			// seq==0 是临时帧（流式增量）：不参与去重、不推进 sent。
			if ev.Seq != 0 && ev.Seq <= sent {
				continue
			}
			if err := sendEvent(ctx, c, ev); err != nil {
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
	// 3) 实时推送。缓冲写满意味着客户端跟不上：明确告知，让客户端带
	// from_seq 重连；不能静默返回，否则连接还在但事件永远不再到达。
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Overflow():
			s.reportOverflow(ctx, c, sub)
			return
		case ev, ok := <-sub.C():
			if !ok {
				s.reportOverflow(ctx, c, sub)
				return
			}
			if ev.Seq != 0 && ev.Seq <= sent {
				continue
			}
			if err := sendEvent(ctx, c, ev); err != nil {
				return
			}
			if ev.Seq != 0 {
				sent = ev.Seq
			}
		}
	}
}

// reportOverflow 只在确实是溢出导致通道关闭时发错误帧；正常取消（ctx 结束、
// 服务端关闭）不打扰客户端。
func (s *Server) reportOverflow(ctx context.Context, c *clientConn, sub *event.Subscription) {
	select {
	case <-sub.Overflow():
		c.sendError(ctx, protocol.CodeInternal, "事件积压超出缓冲，请带 from_seq 重连")
	default:
	}
}

func sendEvent(ctx context.Context, c *clientConn, ev domain.Event) error {
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    string(ev.Type),
		TaskID:  string(ev.TaskID),
		Payload: ev.Payload,
	})
	if err != nil {
		return fmt.Errorf("构造事件帧: %w", err)
	}
	env.SessionID = string(ev.SessionID)
	env.Seq = int64(ev.Seq)
	env.TS = ev.At.UnixMilli()
	if err := c.send(ctx, env); err != nil {
		return fmt.Errorf("发送事件 %s#%d: %w", ev.Type, ev.Seq, err)
	}
	return nil
}

func sessionPayload(sess domain.Session) protocol.SessionPayload {
	return protocol.SessionPayload{
		ID:        string(sess.ID),
		Workspace: sess.Workspace,
		Provider:  sess.Provider,
		Model:     sess.Model,
		Title:     sess.Title,
		CreatedAt: sess.CreatedAt.UnixMilli(),
		UpdatedAt: sess.UpdatedAt.UnixMilli(),
	}
}

func taskPayload(task domain.Task) protocol.TaskPayload {
	return protocol.TaskPayload{
		ID:        string(task.ID),
		SessionID: string(task.SessionID),
		Status:    string(task.Status),
		Prompt:    task.Prompt,
		CreatedAt: task.CreatedAt.UnixMilli(),
	}
}
