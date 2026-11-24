package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/skills"
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
	reply, err := protocol.New(protocol.TypeSessionCreated, s.sessionPayloadWithProject(ctx, sess))
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = string(sess.ID)
	reply.TS = sess.CreatedAt.UnixMilli()
	_ = c.send(ctx, reply)
}

// handleSessionUpdate 切换会话后续任务的供应商/模型。
func (s *Server) handleSessionUpdate(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.SessionUpdatePayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	id := p.SessionID
	if id == "" {
		id = env.SessionID
	}
	if id == "" {
		c.sendError(ctx, protocol.CodeBadRequest, "缺少 session_id")
		return
	}
	if p.Provider != "" && s.cfg != nil {
		if _, ok := s.cfg.Provider(p.Provider); !ok {
			c.sendError(ctx, protocol.CodeBadRequest, fmt.Sprintf("配置里没有供应商 %q", p.Provider))
			return
		}
	}
	sess, err := s.app.UpdateSession(ctx, domain.SessionID(id), p.Provider, p.Model)
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	reply, err := protocol.New(protocol.TypeSessionUpdated, sessionPayload(sess))
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = string(sess.ID)
	reply.TS = sess.UpdatedAt.UnixMilli()
	_ = c.send(ctx, reply)
}

// handleProviderModels 返回该会话可以切换的模型候选。
//
// 配置里显式给了 models 就用它；否则问供应商的 /models。会话当前没有指定
// 供应商时沿用默认供应商。
func (s *Server) handleProviderModels(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil || s.cfg == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入运行时")
		return
	}
	var p protocol.ProviderModelsPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	id := p.SessionID
	if id == "" {
		id = env.SessionID
	}
	sess, err := s.app.GetSession(ctx, domain.SessionID(id))
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	pc, ok := s.cfg.Provider(sess.Provider)
	if sess.Provider == "" {
		pc, ok = s.cfg.DefaultProvider()
	}
	if !ok {
		c.sendError(ctx, protocol.CodeBadRequest, fmt.Sprintf("配置里没有供应商 %q", sess.Provider))
		return
	}

	models := make([]string, 0, len(pc.Models))
	source := "api"
	if len(pc.Models) > 0 {
		source = "config"
		seen := make(map[string]struct{}, len(pc.Models))
		for _, m := range pc.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			models = append(models, m)
		}
	} else {
		built, err := llm.New(pc)
		if err != nil {
			c.sendError(ctx, protocol.CodeInternal, err.Error())
			return
		}
		lister, ok := built.(llm.ModelLister)
		if !ok {
			c.sendError(ctx, protocol.CodeUnsupported, llm.ErrModelsUnsupported.Error())
			return
		}
		mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		models, err = lister.Models(mctx)
		if err != nil {
			c.sendError(ctx, protocol.CodeInternal, err.Error())
			return
		}
	}
	reply, err := protocol.New(protocol.TypeProviderModelsResult, protocol.ProviderModelsResultPayload{
		Models: models,
		Source: source,
	})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = id
	_ = c.send(ctx, reply)
}

func (s *Server) handleMCPList(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入运行时")
		return
	}
	var p protocol.MCPListPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	id := p.SessionID
	if id == "" {
		id = env.SessionID
	}
	if id == "" {
		c.sendError(ctx, protocol.CodeBadRequest, "缺少 session_id")
		return
	}
	sess, err := s.app.GetSession(ctx, domain.SessionID(id))
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	// 只读快照：会话当前工作区里会用到的那几台，不启动任何进程。
	status := s.app.MCPServers(sess.Workspace)
	out := make([]protocol.MCPServerInfo, 0, len(status))
	for _, st := range status {
		out = append(out, protocol.MCPServerInfo{
			Name:      st.Name,
			Command:   strings.Join(st.Argv, " "),
			Dir:       st.Dir,
			Trust:     st.Trust,
			Source:    st.Source,
			Connected: st.Connected,
			Version:   st.Version,
			Tools:     st.Tools,
			TimeoutMS: int(st.Timeout / time.Millisecond),
		})
	}
	reply, err := protocol.New(protocol.TypeMCPListResult, protocol.MCPListResultPayload{Servers: out})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = id
	_ = c.send(ctx, reply)
}

func (s *Server) handleSkillList(ctx context.Context, c *clientConn, env protocol.Envelope) {
	if s.app == nil || s.cfg == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入运行时")
		return
	}
	var p protocol.SkillListPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	id := p.SessionID
	if id == "" {
		id = env.SessionID
	}
	if id == "" {
		c.sendError(ctx, protocol.CodeBadRequest, "缺少 session_id")
		return
	}
	sess, err := s.app.GetSession(ctx, domain.SessionID(id))
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	// 技能目录按工作区解析（相对目录相对工作区根），所以这里要会话。
	// 列表本身很便宜：65 个技能只读每个 SKILL.md 的前 16 KB。
	catalog, errs := skills.Load(s.cfg.Agent.SkillDirsFor(), sess.Workspace)
	for _, err := range errs {
		s.log.Warn("加载技能失败，已跳过", "error", err)
	}
	all := catalog.Skills()
	out := make([]protocol.SkillInfo, 0, len(all))
	for _, sk := range all {
		out = append(out, protocol.SkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
			Manual:      sk.Manual,
		})
	}
	reply, err := protocol.New(protocol.TypeSkillListResult, protocol.SkillListResultPayload{Skills: out})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = id
	_ = c.send(ctx, reply)
}

func (s *Server) sessionPayloadWithProject(ctx context.Context, sess domain.Session) protocol.SessionPayload {
	payload := sessionPayload(sess)
	status, err := s.app.ProjectStatus(ctx, sess.Workspace, s.projectMode())
	if err != nil {
		s.log.Warn("读取项目级扩展状态失败", "workspace", sess.Workspace, "error", err)
		return payload
	}
	if !status.Detected {
		return payload
	}
	info := projectInfo(status)
	payload.Project = &info
	return payload
}

func projectInfo(status app.ProjectStatus) protocol.ProjectInfo {
	return protocol.ProjectInfo{
		Mode:     status.Mode,
		Detected: status.Detected,
		Trusted:  status.Trusted,
		Loaded:   status.Loaded,
		Digest:   status.Digest,
		Summary:  status.Summary,
		Prompts:  status.Prompts,
		Skills:   status.Skills,
		Servers:  status.Servers,
		Error:    status.Err,
	}
}

// projectMode 返回项目级扩展的生效模式；未配置时按 ask 处理。
func (s *Server) projectMode() string {
	if s.cfg == nil || s.cfg.Project.Mode == "" {
		return config.ProjectAsk
	}
	return s.cfg.Project.Mode
}

// handleProjectTrust 确认当前工作区的项目级扩展。
func (s *Server) handleProjectTrust(ctx context.Context, c *clientConn, env protocol.Envelope) {
	s.handleProjectAction(ctx, c, env, true)
}

// handleProjectRevoke 撤销确认。
func (s *Server) handleProjectRevoke(ctx context.Context, c *clientConn, env protocol.Envelope) {
	s.handleProjectAction(ctx, c, env, false)
}

func (s *Server) handleProjectAction(ctx context.Context, c *clientConn, env protocol.Envelope, trust bool) {
	if s.app == nil {
		c.sendError(ctx, protocol.CodeUnsupported, "服务端尚未接入会话存储")
		return
	}
	var p protocol.ProjectActionPayload
	if err := env.Decode(&p); err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	id := p.SessionID
	if id == "" {
		id = env.SessionID
	}
	if id == "" {
		c.sendError(ctx, protocol.CodeBadRequest, "缺少 session_id")
		return
	}
	sess, err := s.app.GetSession(ctx, domain.SessionID(id))
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	var status app.ProjectStatus
	if trust {
		status, err = s.app.TrustProject(ctx, sess.Workspace, s.projectMode())
	} else {
		err = s.app.RevokeProjectTrust(ctx, sess.Workspace)
		if err == nil {
			status, err = s.app.ProjectStatus(ctx, sess.Workspace, s.projectMode())
		}
	}
	if err != nil {
		c.sendError(ctx, protocol.CodeBadRequest, err.Error())
		return
	}
	reply, err := protocol.New(protocol.TypeProjectTrusted, protocol.ProjectStatusPayload{
		SessionID: id,
		Project:   projectInfo(status),
	})
	if err != nil {
		c.sendError(ctx, protocol.CodeInternal, err.Error())
		return
	}
	reply.SessionID = id
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
	// 单次模型覆盖要按配置校验：名字写错就报错，而不是回落到会话默认后
	// 悄悄用另一个模型跑，事后审计才发现。
	if p.Provider != "" && s.cfg != nil {
		if _, ok := s.cfg.Provider(p.Provider); !ok {
			c.sendError(ctx, protocol.CodeBadRequest, fmt.Sprintf("配置里没有供应商 %q", p.Provider))
			return
		}
	}
	task, err := s.app.SubmitTaskWith(ctx, domain.SessionID(p.SessionID), p.Text, app.SubmitOptions{
		Skills:   p.Skills,
		Provider: p.Provider,
		Model:    p.Model,
	})
	if err != nil {
		// 排队已满是可恢复的客户端错误：界面该提示稍后重试，而不是当成协议错误。
		code := protocol.CodeBadRequest
		if errors.Is(err, app.ErrQueueFull) {
			code = protocol.CodeQueueFull
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
		domain.ApprovalID(p.ApprovalID), domain.Decision(p.Decision), p.Note, p.Hunks...); err != nil {
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
	s.log.Info("事件订阅已建立", "session", sessionID, "from_seq", p.FromSeq)
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
			// 同一连接上换了订阅时，旧推送的 ctx 会被取消：Replay 立刻返回
			// ctx 取消，而连接还归新订阅使用。此时往连接里写错误帧会让
			// coder/websocket 直接关连接，客户端看到 connection reset by peer。
			if ctx.Err() != nil {
				return
			}
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

	// 2) 增量订阅补快照：approval.requested 如果发生在 from 之前，回放永远
	// 不会补到它。全量回放（from=0）本身就包含待审批，不需要快照。
	if from > 0 {
		if err := s.sendSnapshot(ctx, c, sessionID, from); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.sendError(ctx, protocol.CodeInternal, err.Error())
			return
		}
	}

	// 3) 清空回放期间进入缓冲的事件；已发送过的 seq 直接丢弃。
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
	// 4) 实时推送。缓冲写满意味着客户端跟不上：明确告知，让客户端带
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

// sendSnapshot 给增量订阅补发待审批快照。
//
// from 超过服务端 head 说明客户端的 seq 来自另一份日志（数据库被重建、换了
// 服务端）：此时不猜，直接要求它从 0 全量重放。
func (s *Server) sendSnapshot(ctx context.Context, c *clientConn, sessionID domain.SessionID, from domain.Seq) error {
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
	env, err := protocol.New(protocol.TypeEventSnapshot, payload)
	if err != nil {
		return err
	}
	// 没有待审批也没有要求重置时不发空帧：快照是补缺口，不是每次重连的
	// 固定握手，否则所有增量订阅都要多处理一个无用帧。
	if !payload.Reset && len(payload.Pending) == 0 {
		return nil
	}
	env.SessionID = string(sessionID)
	return c.send(context.WithoutCancel(ctx), env)
}

// reportOverflow 只在确实是溢出导致通道关闭时发错误帧；正常取消（ctx 结束、
// 服务端关闭）不打扰客户端。
func (s *Server) reportOverflow(ctx context.Context, c *clientConn, sub *event.Subscription) {
	select {
	case <-sub.Overflow():
		// 同上：溢出提示也不能被订阅取消打断，否则同样会关连接。
		c.sendError(context.WithoutCancel(ctx), protocol.CodeInternal, "事件积压超出缓冲，请带 from_seq 重连")
	default:
	}
}

func sendEvent(ctx context.Context, c *clientConn, ev domain.Event) error {
	// 订阅已被取消时不要再写：写失败会被 coder/websocket 当成致命错误并
	// 关闭整条连接，而这条连接可能已经被新订阅接管。
	if err := ctx.Err(); err != nil {
		return err
	}
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
	// 写帧不跟随订阅取消：cancel 只该让循环下一轮退出。若让写也用取消后的
	// ctx，wsjson.Write 会失败，coder/websocket 视之为致命错误直接关掉整条
	// 连接——而这条连接可能已经被同连接上的新订阅接管。
	if err := c.send(context.WithoutCancel(ctx), env); err != nil {
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
