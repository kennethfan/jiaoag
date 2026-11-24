// Package protocol 定义 jiaoag 客户端与服务端之间的线上契约。
//
// 本包只依赖标准库，不得 import 任何其他 internal 包：两端都靠它编译，
// 任何业务耦合都会把服务端实现泄漏进客户端。
package protocol

import (
	"encoding/json"
	"fmt"
)

// Version 是当前协议版本。两端不一致时客户端必须拒绝连接，而不是尝试降级。
const Version = 1

// 信封类型。
const (
	TypeHello       = "hello"        // S→C：连接建立后的第一帧
	TypeClientHello = "client.hello" // C→S：客户端自报版本
	TypePing        = "ping"         // C→S
	TypePong        = "pong"         // S→C
	TypeError       = "error"        // S→C
	TypeEvent       = "event"        // S→C：会话事件（M1 起使用）

	TypeSessionCreate     = "session.create"      // C→S
	TypeSessionCreated    = "session.created"     // S→C
	TypeSessionUpdate     = "session.update"      // C→S：切换本会话使用的供应商/模型
	TypeSessionUpdated    = "session.updated"     // S→C
	TypeSessionList       = "session.list"        // C→S
	TypeSessionListResult = "session.list.result" // S→C
	TypeProviderModels    = "provider.models"     // C→S：列出该会话供应商的可用模型
	// TypeProviderModelsResult 是 provider.models 的响应。
	TypeProviderModelsResult = "provider.models.result" // S→C
	TypeSkillList            = "skill.list"             // C→S：列出技能库（供 /skill 面板）
	TypeSkillListResult      = "skill.list.result"      // S→C
	TypeMCPList              = "mcp.list"               // C→S：列出 MCP server 与运行状态（供 /mcp 面板）
	TypeMCPListResult        = "mcp.list.result"        // S→C
	// TypeAuditTail 是只读的审计增量拉取：按 audit.id 游标向后取，供
	// jiaoag-events 之类的导出/监控客户端使用。after_id 为负表示只取
	// 当前水位（不返回记录，只给 next_id）。
	TypeAuditTail       = "audit.tail"        // C→S
	TypeAuditTailResult = "audit.tail.result" // S→C
	// TypeEventHead 返回会话当前最后的事件序号，供消费者只订阅新事件。
	TypeEventHead       = "event.head"        // C→S
	TypeEventHeadResult = "event.head.result" // S→C
	// TypeProjectTrust 确认当前工作区的项目级扩展（AGENTS.md、.jiaoag/、
	// .agents/skills）；TypeProjectRevoke 撤销确认。
	TypeProjectTrust    = "project.trust"    // C→S
	TypeProjectRevoke   = "project.revoke"   // C→S
	TypeProjectTrusted  = "project.trusted"  // S→C：确认/撤销后的最新状态
	TypeTaskSubmit      = "task.submit"      // C→S
	TypeTaskQueued      = "task.queued"      // S→C
	TypeTaskCancel      = "task.cancel"      // C→S：取消排队中或运行中的任务
	TypeTaskCanceled    = "task.canceled"    // S→C：取消请求已被接受
	TypeApprovalResolve = "approval.resolve" // C→S：对一次待审批给出裁决
	TypeEventSubscribe  = "event.subscribe"  // C→S
	TypeEventSnapshot   = "event.snapshot"   // S→C：增量订阅时补发非事件态（待审批）
)

// 错误码。
const (
	CodeUnauthorized = "unauthorized"
	// CodeForbidden 表示令牌有效但权限不够，例如只读令牌发起写命令。
	CodeForbidden        = "forbidden"
	CodeBadRequest       = "bad_request"
	CodeProtocolMismatch = "protocol_mismatch"
	CodeUnsupported      = "unsupported_type"
	CodeSessionBusy      = "session_busy"
	// CodeQueueFull 表示单会话排队任务已满，客户端稍后重试即可。
	CodeQueueFull = "queue_full"
	CodeInternal  = "internal"
)

// Envelope 是所有帧的统一信封；Payload 的具体类型由 Type 决定。
type Envelope struct {
	Version int             `json:"v"`
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`

	// 以下字段只出现在服务端事件上。
	SessionID string `json:"session_id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	TS        int64  `json:"ts,omitempty"`
}

// New 构造一个信封，payload 为 nil 时省略载荷。
func New(typ string, payload any) (Envelope, error) {
	env := Envelope{Version: Version, Type: typ}
	if payload == nil {
		return env, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("编码 %s 载荷失败: %w", typ, err)
	}
	env.Payload = raw
	return env, nil
}

// Decode 把载荷解码进 v。
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("消息 %s 缺少载荷", e.Type)
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("解码 %s 载荷失败: %w", e.Type, err)
	}
	return nil
}

// HelloPayload 是服务端在连接建立后立即下发的问候帧。
type HelloPayload struct {
	ServerVersion   string `json:"server_version"`
	ProtocolVersion int    `json:"protocol_version"`
	StartedAt       int64  `json:"started_at"`
	Listen          string `json:"listen,omitempty"`
}

// ClientHelloPayload 是客户端自报版本。
type ClientHelloPayload struct {
	ClientVersion   string `json:"client_version"`
	ProtocolVersion int    `json:"protocol_version"`
}

// ErrorPayload 是服务端错误帧的载荷。
type ErrorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// SessionCreatePayload 请求创建一个工作区会话。
type SessionCreatePayload struct {
	Workspace string `json:"workspace"`
	// Roots 是只读附加根（ADR-0036）：只能读，写入与命令仍限主根。
	Roots    []SessionRootPayload `json:"roots,omitempty"`
	Provider string               `json:"provider,omitempty"`
	Model    string               `json:"model,omitempty"`
	Title    string               `json:"title,omitempty"`
}

// SessionUpdatePayload 切换会话后续任务使用的供应商与模型。
// Provider 为空表示沿用当前供应商。
type SessionUpdatePayload struct {
	SessionID string `json:"session_id"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model"`
}

// ProviderModelsPayload 请求列出某个会话所属供应商的可用模型。
type ProviderModelsPayload struct {
	SessionID string `json:"session_id"`
}

// ProviderModelsResultPayload 是模型候选列表。Source 说明来源：
// config（配置里写死的 models）或 api（供应商 /models 接口）。
type ProviderModelsResultPayload struct {
	Models []string `json:"models"`
	Source string   `json:"source,omitempty"`
}

// SessionPayload 是会话的线上表示。
// SessionRootPayload 是会话挂载的只读附加根（ADR-0036）。
type SessionRootPayload struct {
	Alias string `json:"alias"`
	Path  string `json:"path"`
}

type SessionPayload struct {
	ID        string               `json:"id"`
	Workspace string               `json:"workspace"`
	Roots     []SessionRootPayload `json:"roots,omitempty"`
	Provider  string               `json:"provider,omitempty"`
	Model     string               `json:"model,omitempty"`
	Title     string               `json:"title,omitempty"`
	CreatedAt int64                `json:"created_at,omitempty"`
	UpdatedAt int64                `json:"updated_at,omitempty"`
	// Project 只在 session.created 上出现：工作区里有项目级扩展时带上状态，
	// 让客户端能提示「发现了但未确认」。
	Project *ProjectInfo `json:"project,omitempty"`
}

// ProjectInfo 是项目级扩展的状态与摘要（ADR-0025）。
type ProjectInfo struct {
	Mode     string   `json:"mode,omitempty"`
	Detected bool     `json:"detected"`
	Trusted  bool     `json:"trusted"`
	Loaded   bool     `json:"loaded"`
	Digest   string   `json:"digest,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Prompts  []string `json:"prompts,omitempty"`
	Skills   []string `json:"skills,omitempty"`
	Servers  []string `json:"servers,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// ProjectActionPayload 是 project.trust / project.revoke 的请求体。
type ProjectActionPayload struct {
	SessionID string `json:"session_id"`
}

// ProjectStatusPayload 是 project.trusted 的响应。
type ProjectStatusPayload struct {
	SessionID string      `json:"session_id"`
	Project   ProjectInfo `json:"project"`
}

// SessionListResultPayload 是 session.list 的响应。
type SessionListResultPayload struct {
	Sessions []SessionPayload `json:"sessions"`
}

// TaskSubmitPayload 提交一个任务。
type TaskSubmitPayload struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	// Skills 是用户显式点名的技能：服务端会把正文直接注入这次任务，
	// 不依赖模型自己决定要不要加载。
	Skills []string `json:"skills,omitempty"`
	// Provider/Model 只覆盖这一次任务；空表示跟随会话当前设置。
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// SkillListPayload 请求技能库列表。
type SkillListPayload struct {
	SessionID string `json:"session_id"`
}

// SkillInfo 是技能列表里的一项。
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Manual 表示 SKILL.md 声明了 disable-model-invocation：模型不会自动挑，
	// 只能由用户显式指定。
	Manual bool `json:"manual,omitempty"`
}

// SkillListResultPayload 是 skill.list 的响应。
type SkillListResultPayload struct {
	Skills []SkillInfo `json:"skills"`
}

// MCPListPayload 请求当前工作区的 MCP server 列表。
type MCPListPayload struct {
	SessionID string `json:"session_id"`
}

// MCPServerInfo 是一台 MCP server 的线上表示。只读：面板据此展示，不能改。
type MCPServerInfo struct {
	Name string `json:"name"`
	// Command 是完整启动命令（argv 拼一行），方便认出这台 server 是什么。
	Command string `json:"command"`
	Dir     string `json:"dir,omitempty"`
	// Trust 为真表示配置即信任、调用免审批（仍记审计）；为假则逐次审批。
	Trust bool `json:"trust"`
	// Source 是来源：config（全局配置）或 project（工作区的 .jiaoag/mcp.yaml）。
	Source string `json:"source"`
	// Connected 表示该工作区里它的子进程已经连上过；未用到时是 false。
	Connected bool   `json:"connected"`
	Version   string `json:"version,omitempty"`
	Tools     int    `json:"tools"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// MCPListResultPayload 是 mcp.list 的响应。
type MCPListResultPayload struct {
	Servers []MCPServerInfo `json:"servers"`
}

// AuditTailPayload 请求 after_id 之后的审计记录。
//
// AfterID 为负时不返回记录，只回当前水位（next_id）——tail 的「从现在开始」
// 语义靠它实现，避免把历史整库倒出来。
type AuditTailPayload struct {
	AfterID int64 `json:"after_id"`
	Limit   int   `json:"limit,omitempty"`
}

// AuditRecordPayload 是一条审计记录的线上表示。只读、不可改。
type AuditRecordPayload struct {
	ID         int64  `json:"id"`
	At         int64  `json:"at"` // Unix 毫秒
	Actor      string `json:"actor"`
	Action     string `json:"action"`
	TaskID     string `json:"task_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Detail 是原始 JSON，字段随 action 变化（argv、路径、策略版本等）。
	Detail json.RawMessage `json:"detail,omitempty"`
}

// AuditTailResultPayload 是 audit.tail 的响应。
type AuditTailResultPayload struct {
	Records []AuditRecordPayload `json:"records"`
	// NextID 是下次请求该带的 after_id；没有记录时等于当前水位。
	NextID int64 `json:"next_id"`
}

// EventHeadPayload 查询某个会话当前最后的事件序号。
type EventHeadPayload struct {
	SessionID string `json:"session_id"`
}

// EventHeadResultPayload 是 event.head 的响应。
type EventHeadResultPayload struct {
	Seq int64 `json:"seq"`
}

// TaskCancelPayload 请求取消一个任务。任务已终止时是幂等的空操作。
type TaskCancelPayload struct {
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
}

// TaskPayload 是任务的线上表示。
type TaskPayload struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Prompt    string `json:"prompt,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// EventSubscribePayload 请求从 from_seq 之后开始订阅事件。
// 服务端顺序固定为：先订阅进缓冲 → 回放 → 刷缓冲按 seq 去重。
type EventSubscribePayload struct {
	SessionID string `json:"session_id"`
	FromSeq   int64  `json:"from_seq"`
}

// StreamEventPayload 是 event 帧的载荷：信封带 seq 与 session_id，
// 载荷只描述事件本身。
type StreamEventPayload struct {
	Kind    string          `json:"kind"`
	TaskID  string          `json:"task_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// EventSnapshotPayload 是增量订阅（from_seq>0）在回放之后补发的一次快照。
//
// 事件日志能重建历史，但重建不了「断线前就已经发生、之后不会再产生事件」
// 的状态：待审批的 approval.requested 早于 from_seq，错过就永远不会再出现。
// 快照只补这类非事件态；它不推进 seq，也不替代事件。
type EventSnapshotPayload struct {
	// HeadSeq 是快照时刻会话最后一条事件的 seq，供客户端判断自己的
	// from_seq 是否已经越过服务端（数据库被重建等）。
	HeadSeq int64 `json:"head_seq"`
	// Reset 为真表示 from_seq 超过 HeadSeq：客户端必须清空本地视图、
	// 从 0 重新订阅。此时 Pending 为空。
	Reset bool `json:"reset,omitempty"`
	// Pending 是仍然待裁决的 approval.requested 原始载荷（按请求顺序）。
	Pending []StreamEventPayload `json:"pending,omitempty"`
}

// ApprovalResolvePayload 是用户对一次待审批的裁决。
//
// 服务端只回一条 approval.resolved 事件（不是回执帧）：裁决的结果是事件日志
// 的一部分，多一条回执帧就等于给同一件事开两条投递路径，迟早不一致。
type ApprovalResolvePayload struct {
	SessionID  string `json:"session_id"`
	ApprovalID string `json:"approval_id"`
	// Decision 取 allow_once | allow_session | deny。
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
	// Hunks 是要应用的块下标（0 起）。省略表示整份 diff；显式给出时只有这些块
	// 会被写入。allow_session 与 hunks 同时出现时按 allow_once 处理：部分应用
	// 是一次性决定，不该变成「同类请求以后都放行」。
	Hunks []int `json:"hunks,omitempty"`
}
