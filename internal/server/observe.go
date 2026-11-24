package server

import (
	"context"
	"errors"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// handleAuditTail 是审计的只读导出通道：按 audit.id 游标向后取记录。
//
// 只读是刻意的：审计是事后追责的依据，导出侧不能顺带改任何东西；也没有
// 「删除审计」的入口。after_id 为负表示只取当前水位（tail 的「从现在开始」）。
func (s *Server) handleAuditTail(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.AuditTailPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}

	if p.AfterID < 0 {
		head, err := s.app.AuditHead(ctx)
		if err != nil {
			c.sendError(ctx, protocol.CodeInternal, err.Error())
			return
		}
		s.replyAudit(ctx, c, protocol.AuditTailResultPayload{
			Records: []protocol.AuditRecordPayload{},
			NextID:  head,
		})
		return
	}

	records, err := s.app.ListAudit(ctx, p.AfterID, p.Limit)
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	out := make([]protocol.AuditRecordPayload, 0, len(records))
	next := p.AfterID
	for _, r := range records {
		out = append(out, auditRecordPayload(r))
		if r.ID > next {
			next = r.ID
		}
	}
	s.replyAudit(ctx, c, protocol.AuditTailResultPayload{Records: out, NextID: next})
}

// handleEventHead 返回会话当前最后的事件序号，供消费者只订阅新事件。
func (s *Server) handleEventHead(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.EventHeadPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	sessionID := domain.SessionID(p.SessionID)
	if !sessionID.Valid() {
		c.sendError(ctx, protocol.CodeBadRequest, "event.head 缺少 session_id")
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
	seq, err := s.app.LastSeq(ctx, sessionID)
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply, err := protocol.New(protocol.TypeEventHeadResult, protocol.EventHeadResultPayload{Seq: int64(seq)})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = string(sessionID)
	_ = c.send(ctx, reply)
}

func (s *Server) replyAudit(ctx context.Context, c *clientConn, payload protocol.AuditTailResultPayload) {
	reply, err := protocol.New(protocol.TypeAuditTailResult, payload)
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	_ = c.send(ctx, reply)
}

func auditRecordPayload(r domain.AuditRecord) protocol.AuditRecordPayload {
	return protocol.AuditRecordPayload{
		ID:         r.ID,
		At:         r.At.UnixMilli(),
		Actor:      r.Actor,
		Action:     r.Action,
		TaskID:     string(r.TaskID),
		SessionID:  string(r.SessionID),
		ToolCallID: string(r.ToolCallID),
		Detail:     r.Detail,
	}
}
