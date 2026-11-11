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
)

// 错误码。
const (
	CodeUnauthorized     = "unauthorized"
	CodeBadRequest       = "bad_request"
	CodeProtocolMismatch = "protocol_mismatch"
	CodeUnsupported      = "unsupported_type"
	CodeInternal         = "internal"
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
