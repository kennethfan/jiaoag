package server

import (
	"context"
	"time"

	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
)

// 本文件是 WS 传输适配器：只做「解帧 → 调契约 → 编码回执」。
// 全部业务语义在 internal/rpc，TUI 的进程内模式直接调同一组方法。

func (c *clientConn) principal() rpc.Principal {
	return rpc.Principal{TokenName: c.tokenName, ReadOnly: c.readOnly}
}

// decode 解载荷；失败时回 bad_request 并返回 false。
func (s *Server) decode(ctx context.Context, c *clientConn, env protocol.Envelope, out any) bool {
	if err := env.Decode(out); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return false
	}
	return true
}

// reply 构造回执帧。sessionID 与时间戳是信封层的元数据，统一在这里补。
func (s *Server) reply(ctx context.Context, c *clientConn, typ string, payload any, sessionID string) {
	env, err := protocol.New(typ, payload)
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	env.SessionID = sessionID
	env.TS = time.Now().UnixMilli()
	_ = c.send(ctx, env)
}

func (s *Server) fail(ctx context.Context, c *clientConn, err error) {
	c.sendError(ctx, rpc.CodeOf(err), err.Error())
}

// requireCore 在所有命令前调用：没有接入 app 时统一回 unsupported。
func (s *Server) requireCore(ctx context.Context, c *clientConn) bool {
	if s.core == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入运行时")
		return false
	}
	return true
}

func (s *Server) handleSessionCreate(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.SessionCreatePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	out, err := s.core.SessionCreate(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeSessionCreated, out, out.ID)
}

func (s *Server) handleSessionUpdate(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.SessionUpdatePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	out, err := s.core.SessionUpdate(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeSessionUpdated, out, out.ID)
}

func (s *Server) handleProviderModels(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.ProviderModelsPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	out, err := s.core.ProviderModels(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeProviderModelsResult, out, p.SessionID)
}

func (s *Server) handleMCPList(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.MCPListPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	out, err := s.core.MCPList(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeMCPListResult, out, p.SessionID)
}

func (s *Server) handleSkillList(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.SkillListPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	out, err := s.core.SkillList(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeSkillListResult, out, p.SessionID)
}

func (s *Server) handleProjectTrust(ctx context.Context, c *clientConn, env protocol.Envelope) {
	s.handleProjectAction(ctx, c, env, true)
}

func (s *Server) handleProjectRevoke(ctx context.Context, c *clientConn, env protocol.Envelope) {
	s.handleProjectAction(ctx, c, env, false)
}

func (s *Server) handleProjectAction(ctx context.Context, c *clientConn, env protocol.Envelope, trust bool) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.ProjectActionPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	var (
		out protocol.ProjectStatusPayload
		err error
	)
	if trust {
		out, err = s.core.ProjectTrust(ctx, c.principal(), p)
	} else {
		out, err = s.core.ProjectRevoke(ctx, c.principal(), p)
	}
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeProjectTrusted, out, p.SessionID)
}

func (s *Server) handleSessionList(ctx context.Context, c *clientConn) {
	if !s.requireCore(ctx, c) {
		return
	}
	out, err := s.core.SessionList(ctx, c.principal())
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeSessionListResult, out, "")
}

func (s *Server) handleSessionManage(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.SessionManagePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	out, err := s.core.SessionManage(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeSessionManageResult, out, p.SessionID)
}

func (s *Server) handleSessionRename(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.SessionRenamePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	out, err := s.core.SessionRename(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeSessionRenamed, out, p.SessionID)
}

func (s *Server) handleSessionDelete(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.SessionDeletePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	if err := s.core.SessionDelete(ctx, c.principal(), p); err != nil {
		s.fail(ctx, c, err)
		return
	}
	// 删除的正好是本连接订阅的会话：先停掉推送，否则它会一直等一个不再有
	// 事件可发的订阅。
	c.cancelStream(p.SessionID)
	s.reply(ctx, c, protocol.TypeSessionDeleted, protocol.SessionDeletedPayload(p), p.SessionID)
}

func (s *Server) handleEventUnsubscribe(ctx context.Context, c *clientConn, env protocol.Envelope) {
	var p protocol.EventUnsubscribePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if p.SessionID == "" {
		p.SessionID = env.SessionID
	}
	if p.SessionID == "" {
		c.sendError(ctx, protocol.CodeBadRequest, "event.unsubscribe 缺少 session_id")
		return
	}
	c.cancelStream(p.SessionID)
}

func (s *Server) handleTaskSubmit(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.TaskSubmitPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	out, err := s.core.TaskSubmit(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeTaskQueued, out, out.SessionID)
}

func (s *Server) handleTaskCancel(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.TaskCancelPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	out, err := s.core.TaskCancel(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeTaskCanceled, out, out.SessionID)
}

// handleApprovalResolve 不回执帧：裁决结果由 agent 写成 approval.resolved 事件。
func (s *Server) handleApprovalResolve(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.ApprovalResolvePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	if err := s.core.ApprovalResolve(ctx, c.principal(), p); err != nil {
		s.fail(ctx, c, err)
	}
}

func (s *Server) handleEventSubscribe(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.EventSubscribePayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	ch, cancel, err := s.streams.Subscribe(ctx, p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	key := p.SessionID
	s.log.Info("事件订阅已建立", "session", key, "from_seq", p.FromSeq)
	handle := c.beginStream(key, cancel)
	go func() {
		defer c.endStream(key, handle)
		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-ch:
				if !ok {
					return
				}
				if err := c.send(context.WithoutCancel(ctx), env); err != nil {
					return
				}
			}
		}
	}()
}

func (s *Server) handleAuditTail(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.AuditTailPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	out, err := s.core.AuditTail(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeAuditTailResult, out, "")
}

func (s *Server) handleEventHead(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if !s.requireCore(ctx, c) {
		return
	}
	var p protocol.EventHeadPayload
	if !s.decode(ctx, c, env, &p) {
		return
	}
	out, err := s.core.EventHead(ctx, c.principal(), p)
	if err != nil {
		s.fail(ctx, c, err)
		return
	}
	s.reply(ctx, c, protocol.TypeEventHeadResult, out, p.SessionID)
}
