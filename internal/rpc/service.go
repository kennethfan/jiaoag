package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/skills"
)

// Core 是 Service 的唯一实现：持有 app 与配置，供 WS 适配器和 TUI 共用。
type Core struct {
	app *app.App
	cfg *config.Config
	log *slog.Logger
}

func NewCore(a *app.App, cfg *config.Config, log *slog.Logger) *Core {
	return &Core{app: a, cfg: cfg, log: log}
}

// guardWrite 在读方法里不该被调用；写方法统一先过这一层，
// 这样「只读令牌不能改」的规则不依赖传输层记得拦。
func (c *Core) guardWrite(p Principal, method string) error {
	if p.ReadOnly {
		return forbidden(method)
	}
	return nil
}

func (c *Core) SessionCreate(ctx context.Context, p Principal, in protocol.SessionCreatePayload) (protocol.SessionPayload, error) {
	if err := c.guardWrite(p, protocol.TypeSessionCreate); err != nil {
		return protocol.SessionPayload{}, err
	}
	if c.app == nil {
		return protocol.SessionPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	sess, err := c.app.CreateSession(ctx, in.Workspace, in.Provider, in.Model, in.Title, sessionRootsDomain(in.Roots)...)
	if err != nil {
		return protocol.SessionPayload{}, badRequest("%s", err)
	}
	return c.sessionPayloadWithProject(ctx, sess), nil
}

func (c *Core) SessionUpdate(ctx context.Context, p Principal, in protocol.SessionUpdatePayload) (protocol.SessionPayload, error) {
	if err := c.guardWrite(p, protocol.TypeSessionUpdate); err != nil {
		return protocol.SessionPayload{}, err
	}
	if c.app == nil {
		return protocol.SessionPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	id := in.SessionID
	if id == "" {
		return protocol.SessionPayload{}, badRequest("缺少 session_id")
	}
	if in.Provider != "" && c.cfg != nil {
		if _, ok := c.cfg.Provider(in.Provider); !ok {
			return protocol.SessionPayload{}, badRequest("配置里没有供应商 %q", in.Provider)
		}
	}
	sess, err := c.app.UpdateSession(ctx, domain.SessionID(id), in.Provider, in.Model)
	if err != nil {
		return protocol.SessionPayload{}, badRequest("%s", err)
	}
	return sessionPayload(sess), nil
}

func (c *Core) SessionManage(ctx context.Context, p Principal, in protocol.SessionManagePayload) (protocol.SessionManageResultPayload, error) {
	if c.app == nil {
		return protocol.SessionManageResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	var workspace string
	switch {
	case in.SessionID != "":
		sess, err := c.app.GetSession(ctx, domain.SessionID(in.SessionID))
		if err != nil {
			return protocol.SessionManageResultPayload{}, badRequest("%s", err)
		}
		workspace = sess.Workspace
	case in.All:
		workspace = ""
	case in.Workspace != "":
		abs, err := filepath.Abs(in.Workspace)
		if err != nil {
			return protocol.SessionManageResultPayload{}, badRequest("workspace 无法绝对化: %s", err)
		}
		workspace = abs
	default:
		return protocol.SessionManageResultPayload{}, badRequest("缺少 session_id（或传 workspace / all）")
	}
	summaries, err := c.app.SessionSummaries(ctx, workspace)
	if err != nil {
		return protocol.SessionManageResultPayload{}, internal(err)
	}
	out := make([]protocol.SessionSummaryPayload, 0, len(summaries))
	for _, sum := range summaries {
		out = append(out, protocol.SessionSummaryPayload{
			ID:               string(sum.ID),
			Title:            sum.Title,
			Workspace:        sum.Workspace,
			Provider:         sum.Provider,
			Model:            sum.Model,
			Tasks:            sum.Tasks,
			ActiveTasks:      sum.ActiveTasks,
			PromptTokens:     sum.PromptTokens,
			CompletionTokens: sum.CompletionTokens,
			CreatedAt:        sum.CreatedAt.UnixMilli(),
			UpdatedAt:        sum.UpdatedAt.UnixMilli(),
		})
	}
	return protocol.SessionManageResultPayload{Sessions: out}, nil
}

func (c *Core) SessionRename(ctx context.Context, p Principal, in protocol.SessionRenamePayload) (protocol.SessionPayload, error) {
	if err := c.guardWrite(p, protocol.TypeSessionRename); err != nil {
		return protocol.SessionPayload{}, err
	}
	if c.app == nil {
		return protocol.SessionPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	if in.SessionID == "" {
		return protocol.SessionPayload{}, badRequest("缺少 session_id")
	}
	sess, err := c.app.RenameSession(ctx, domain.SessionID(in.SessionID), in.Title)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return protocol.SessionPayload{}, notFound(err)
		}
		return protocol.SessionPayload{}, internal(err)
	}
	return sessionPayload(sess), nil
}

func (c *Core) SessionDelete(ctx context.Context, p Principal, in protocol.SessionDeletePayload) error {
	if err := c.guardWrite(p, protocol.TypeSessionDelete); err != nil {
		return err
	}
	if c.app == nil {
		return &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	if in.SessionID == "" {
		return badRequest("缺少 session_id")
	}
	if err := c.app.DeleteSession(ctx, domain.SessionID(in.SessionID)); err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrSessionBusy) {
			return notFound(err)
		}
		return internal(err)
	}
	return nil
}

func (c *Core) SessionList(ctx context.Context, p Principal) (protocol.SessionListResultPayload, error) {
	if c.app == nil {
		return protocol.SessionListResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	sessions, err := c.app.ListSessions(ctx)
	if err != nil {
		return protocol.SessionListResultPayload{}, internal(err)
	}
	out := make([]protocol.SessionPayload, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionPayload(sess))
	}
	return protocol.SessionListResultPayload{Sessions: out}, nil
}

func (c *Core) TaskSubmit(ctx context.Context, p Principal, in protocol.TaskSubmitPayload) (protocol.TaskPayload, error) {
	if err := c.guardWrite(p, protocol.TypeTaskSubmit); err != nil {
		return protocol.TaskPayload{}, err
	}
	if c.app == nil {
		return protocol.TaskPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	if in.Provider != "" && c.cfg != nil {
		if _, ok := c.cfg.Provider(in.Provider); !ok {
			return protocol.TaskPayload{}, badRequest("配置里没有供应商 %q", in.Provider)
		}
	}
	task, err := c.app.SubmitTaskWith(ctx, domain.SessionID(in.SessionID), in.Text, app.SubmitOptions{
		Skills:   in.Skills,
		Provider: in.Provider,
		Model:    in.Model,
	})
	if err != nil {
		if errors.Is(err, app.ErrQueueFull) {
			return protocol.TaskPayload{}, &Error{Code: protocol.CodeQueueFull, Message: err.Error()}
		}
		return protocol.TaskPayload{}, badRequest("%s", err)
	}
	return taskPayload(task), nil
}

func (c *Core) TaskCancel(ctx context.Context, p Principal, in protocol.TaskCancelPayload) (protocol.TaskPayload, error) {
	if err := c.guardWrite(p, protocol.TypeTaskCancel); err != nil {
		return protocol.TaskPayload{}, err
	}
	if c.app == nil {
		return protocol.TaskPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	sessionID := domain.SessionID(in.SessionID)
	taskID := domain.TaskID(in.TaskID)
	if !sessionID.Valid() || !taskID.Valid() {
		return protocol.TaskPayload{}, badRequest("task.cancel 需要 session_id 与 task_id")
	}
	if err := c.app.CancelTask(ctx, sessionID, taskID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return protocol.TaskPayload{}, notFound(err)
		}
		return protocol.TaskPayload{}, internal(err)
	}
	return protocol.TaskPayload{ID: in.TaskID, SessionID: in.SessionID, Status: string(domain.TaskCanceled)}, nil
}

// ApprovalResolve 不回执：裁决结果由 agent 写成 approval.resolved 事件。
func (c *Core) ApprovalResolve(ctx context.Context, p Principal, in protocol.ApprovalResolvePayload) error {
	if err := c.guardWrite(p, protocol.TypeApprovalResolve); err != nil {
		return err
	}
	if c.app == nil {
		return &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	if in.SessionID == "" || in.ApprovalID == "" {
		return badRequest("approval.resolve 需要 session_id 与 approval_id")
	}
	switch domain.Decision(in.Decision) {
	case domain.DecisionAllowOnce, domain.DecisionAllowSession, domain.DecisionDeny:
	default:
		return badRequest("approval.resolve 的 decision 必须是 allow_once | allow_session | deny")
	}
	if err := c.app.ResolveApproval(ctx, domain.SessionID(in.SessionID),
		domain.ApprovalID(in.ApprovalID), domain.Decision(in.Decision), in.Note, in.Hunks...); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return notFound(err)
		}
		return internal(err)
	}
	return nil
}

func (c *Core) ProviderModels(ctx context.Context, p Principal, in protocol.ProviderModelsPayload) (protocol.ProviderModelsResultPayload, error) {
	if c.app == nil || c.cfg == nil {
		return protocol.ProviderModelsResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入运行时"}
	}
	if in.SessionID == "" {
		return protocol.ProviderModelsResultPayload{}, badRequest("缺少 session_id")
	}
	sess, err := c.app.GetSession(ctx, domain.SessionID(in.SessionID))
	if err != nil {
		return protocol.ProviderModelsResultPayload{}, badRequest("%s", err)
	}
	pc, ok := c.cfg.Provider(sess.Provider)
	if sess.Provider == "" {
		pc, ok = c.cfg.DefaultProvider()
	}
	if !ok {
		return protocol.ProviderModelsResultPayload{}, badRequest("配置里没有供应商 %q", sess.Provider)
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
			return protocol.ProviderModelsResultPayload{}, internal(err)
		}
		lister, ok := built.(llm.ModelLister)
		if !ok {
			return protocol.ProviderModelsResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: llm.ErrModelsUnsupported.Error()}
		}
		mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		models, err = lister.Models(mctx)
		if err != nil {
			return protocol.ProviderModelsResultPayload{}, internal(err)
		}
	}
	return protocol.ProviderModelsResultPayload{Models: models, Source: source}, nil
}

func (c *Core) SkillList(ctx context.Context, p Principal, in protocol.SkillListPayload) (protocol.SkillListResultPayload, error) {
	if c.app == nil || c.cfg == nil {
		return protocol.SkillListResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入运行时"}
	}
	if in.SessionID == "" {
		return protocol.SkillListResultPayload{}, badRequest("缺少 session_id")
	}
	sess, err := c.app.GetSession(ctx, domain.SessionID(in.SessionID))
	if err != nil {
		return protocol.SkillListResultPayload{}, badRequest("%s", err)
	}
	catalog, errs := skills.Load(c.cfg.Agent.SkillDirsFor(), sess.Workspace)
	for _, err := range errs {
		if c.log != nil {
			c.log.Warn("加载技能失败，已跳过", "error", err)
		}
	}
	all := catalog.Skills()
	out := make([]protocol.SkillInfo, 0, len(all))
	for _, sk := range all {
		out = append(out, protocol.SkillInfo{Name: sk.Name, Description: sk.Description, Manual: sk.Manual})
	}
	return protocol.SkillListResultPayload{Skills: out}, nil
}

func (c *Core) MCPList(ctx context.Context, p Principal, in protocol.MCPListPayload) (protocol.MCPListResultPayload, error) {
	if c.app == nil {
		return protocol.MCPListResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入运行时"}
	}
	if in.SessionID == "" {
		return protocol.MCPListResultPayload{}, badRequest("缺少 session_id")
	}
	sess, err := c.app.GetSession(ctx, domain.SessionID(in.SessionID))
	if err != nil {
		return protocol.MCPListResultPayload{}, badRequest("%s", err)
	}
	status := c.app.MCPServers(sess.Workspace)
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
	return protocol.MCPListResultPayload{Servers: out}, nil
}

func (c *Core) ProjectTrust(ctx context.Context, p Principal, in protocol.ProjectActionPayload) (protocol.ProjectStatusPayload, error) {
	return c.projectAction(ctx, p, in, true)
}

func (c *Core) ProjectRevoke(ctx context.Context, p Principal, in protocol.ProjectActionPayload) (protocol.ProjectStatusPayload, error) {
	return c.projectAction(ctx, p, in, false)
}

func (c *Core) projectAction(ctx context.Context, p Principal, in protocol.ProjectActionPayload, trust bool) (protocol.ProjectStatusPayload, error) {
	if err := c.guardWrite(p, protocol.TypeProjectTrust); err != nil {
		return protocol.ProjectStatusPayload{}, err
	}
	if c.app == nil {
		return protocol.ProjectStatusPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	if in.SessionID == "" {
		return protocol.ProjectStatusPayload{}, badRequest("缺少 session_id")
	}
	sess, err := c.app.GetSession(ctx, domain.SessionID(in.SessionID))
	if err != nil {
		return protocol.ProjectStatusPayload{}, badRequest("%s", err)
	}
	var status app.ProjectStatus
	if trust {
		status, err = c.app.TrustProject(ctx, sess.Workspace, c.projectMode())
	} else {
		err = c.app.RevokeProjectTrust(ctx, sess.Workspace)
		if err == nil {
			status, err = c.app.ProjectStatus(ctx, sess.Workspace, c.projectMode())
		}
	}
	if err != nil {
		return protocol.ProjectStatusPayload{}, badRequest("%s", err)
	}
	return protocol.ProjectStatusPayload{SessionID: in.SessionID, Project: projectInfo(status)}, nil
}

func (c *Core) EventHead(ctx context.Context, p Principal, in protocol.EventHeadPayload) (protocol.EventHeadResultPayload, error) {
	if c.app == nil {
		return protocol.EventHeadResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	sessionID := domain.SessionID(in.SessionID)
	if !sessionID.Valid() {
		return protocol.EventHeadResultPayload{}, badRequest("event.head 缺少 session_id")
	}
	if _, err := c.app.GetSession(ctx, sessionID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return protocol.EventHeadResultPayload{}, notFound(err)
		}
		return protocol.EventHeadResultPayload{}, internal(err)
	}
	seq, err := c.app.LastSeq(ctx, sessionID)
	if err != nil {
		return protocol.EventHeadResultPayload{}, internal(err)
	}
	return protocol.EventHeadResultPayload{Seq: int64(seq)}, nil
}

func (c *Core) AuditTail(ctx context.Context, p Principal, in protocol.AuditTailPayload) (protocol.AuditTailResultPayload, error) {
	if c.app == nil {
		return protocol.AuditTailResultPayload{}, &Error{Code: protocol.CodeUnsupported, Message: "服务端尚未接入会话存储"}
	}
	if in.AfterID < 0 {
		head, err := c.app.AuditHead(ctx)
		if err != nil {
			return protocol.AuditTailResultPayload{}, internal(err)
		}
		return protocol.AuditTailResultPayload{Records: []protocol.AuditRecordPayload{}, NextID: head}, nil
	}
	records, err := c.app.ListAudit(ctx, in.AfterID, in.Limit)
	if err != nil {
		return protocol.AuditTailResultPayload{}, internal(err)
	}
	out := make([]protocol.AuditRecordPayload, 0, len(records))
	next := in.AfterID
	for _, r := range records {
		out = append(out, auditRecordPayload(r))
		if r.ID > next {
			next = r.ID
		}
	}
	return protocol.AuditTailResultPayload{Records: out, NextID: next}, nil
}

// --- 载荷转换（与服务端原来的私有 helper 等价） ---

func (c *Core) sessionPayloadWithProject(ctx context.Context, sess domain.Session) protocol.SessionPayload {
	payload := sessionPayload(sess)
	status, err := c.app.ProjectStatus(ctx, sess.Workspace, c.projectMode())
	if err != nil {
		if c.log != nil {
			c.log.Warn("读取项目级扩展状态失败", "workspace", sess.Workspace, "error", err)
		}
		return payload
	}
	if !status.Detected {
		return payload
	}
	info := projectInfo(status)
	payload.Project = &info
	return payload
}

func (c *Core) projectMode() string {
	if c.cfg == nil || c.cfg.Project.Mode == "" {
		return config.ProjectAsk
	}
	return c.cfg.Project.Mode
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

func sessionRootsDomain(roots []protocol.SessionRootPayload) []domain.SessionRoot {
	if len(roots) == 0 {
		return nil
	}
	out := make([]domain.SessionRoot, 0, len(roots))
	for _, r := range roots {
		out = append(out, domain.SessionRoot{Alias: r.Alias, Path: r.Path})
	}
	return out
}

func sessionRootsPayload(roots []domain.SessionRoot) []protocol.SessionRootPayload {
	if len(roots) == 0 {
		return nil
	}
	out := make([]protocol.SessionRootPayload, 0, len(roots))
	for _, r := range roots {
		out = append(out, protocol.SessionRootPayload{Alias: r.Alias, Path: r.Path})
	}
	return out
}

func sessionPayload(sess domain.Session) protocol.SessionPayload {
	return protocol.SessionPayload{
		ID:        string(sess.ID),
		Workspace: sess.Workspace,
		Roots:     sessionRootsPayload(sess.Roots),
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

var _ Service = (*Core)(nil)

// 让 fmt 保持被引用：错误消息里常用 %s，保留统一格式化入口。
var _ = fmt.Sprintf
