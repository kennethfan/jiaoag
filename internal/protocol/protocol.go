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
	TypeSessionList       = "session.list"        // C→S
	TypeSessionListResult = "session.list.result" // S→C
	TypeTaskSubmit        = "task.submit"         // C→S
	TypeTaskQueued        = "task.queued"         // S→C
	TypeTaskCancel        = "task.cancel"         // C→S：取消排队中或运行中的任务
	TypeTaskCanceled      = "task.canceled"       // S→C：取消请求已被接受
	TypeApprovalResolve   = "approval.resolve"    // C→S：对一次待审批给出裁决
	TypeEventSubscribe    = "event.subscribe"     // C→S
)

// 错误码。
const (
	CodeUnauthorized     = "unauthorized"
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
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Title     string `json:"title,omitempty"`
}

// SessionPayload 是会话的线上表示。
type SessionPayload struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// SessionListResultPayload 是 session.list 的响应。
type SessionListResultPayload struct {
	Sessions []SessionPayload `json:"sessions"`
}

// TaskSubmitPayload 提交一个任务；M1 只入库入队，不执行。
type TaskSubmitPayload struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
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
}
