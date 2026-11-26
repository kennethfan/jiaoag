// Package rpc 是 jiaoag 的契约层：一份强类型的方法定义，供两种传输复用。
//
// 服务端把它翻译成 JSON/WebSocket 帧；TUI 在进程内直接调用同一组方法。
// 协议（载荷类型、错误码、语义、只读限制）只有这一份实现，传输只是薄适配器。
package rpc

import (
	"context"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// Principal 是一次调用的身份。本地调用固定是 owner（可写）；远程调用由
// 握手时的令牌决定，只读令牌不能执行写方法。
type Principal struct {
	TokenName string
	ReadOnly  bool
}

// Owner 是进程内调用的身份：TUI 拥有运行时，权限等同于宿主本身。
func Owner() Principal { return Principal{TokenName: "local"} }

// Error 是契约层唯一向外暴露的错误形态：Code 与线上错误码一致，
// 调用方（WS 适配器或 TUI）不需要再猜该回什么码。
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// CodeOf 把任意错误映射成协议错误码；未知错误一律按内部错误处理。
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return protocol.CodeInternal
}

func badRequest(format string, args ...any) error {
	return &Error{Code: protocol.CodeBadRequest, Message: fmt.Sprintf(format, args...)}
}

func internal(err error) error {
	return &Error{Code: protocol.CodeInternal, Message: err.Error()}
}

func forbidden(method string) error {
	return &Error{Code: protocol.CodeForbidden, Message: "只读令牌不能执行 " + method}
}

func notFound(err error) error {
	return &Error{Code: protocol.CodeBadRequest, Message: err.Error()}
}

// Service 是全部命令的强类型契约。方法返回的载荷直接对应协议里的回执帧。
type Service interface {
	SessionCreate(ctx context.Context, p Principal, in protocol.SessionCreatePayload) (protocol.SessionPayload, error)
	SessionUpdate(ctx context.Context, p Principal, in protocol.SessionUpdatePayload) (protocol.SessionPayload, error)
	SessionManage(ctx context.Context, p Principal, in protocol.SessionManagePayload) (protocol.SessionManageResultPayload, error)
	SessionRename(ctx context.Context, p Principal, in protocol.SessionRenamePayload) (protocol.SessionPayload, error)
	SessionDelete(ctx context.Context, p Principal, in protocol.SessionDeletePayload) error
	SessionList(ctx context.Context, p Principal) (protocol.SessionListResultPayload, error)

	TaskSubmit(ctx context.Context, p Principal, in protocol.TaskSubmitPayload) (protocol.TaskPayload, error)
	TaskCancel(ctx context.Context, p Principal, in protocol.TaskCancelPayload) (protocol.TaskPayload, error)
	ApprovalResolve(ctx context.Context, p Principal, in protocol.ApprovalResolvePayload) error

	ProviderModels(ctx context.Context, p Principal, in protocol.ProviderModelsPayload) (protocol.ProviderModelsResultPayload, error)
	SkillList(ctx context.Context, p Principal, in protocol.SkillListPayload) (protocol.SkillListResultPayload, error)
	MCPList(ctx context.Context, p Principal, in protocol.MCPListPayload) (protocol.MCPListResultPayload, error)
	ProjectTrust(ctx context.Context, p Principal, in protocol.ProjectActionPayload) (protocol.ProjectStatusPayload, error)
	ProjectRevoke(ctx context.Context, p Principal, in protocol.ProjectActionPayload) (protocol.ProjectStatusPayload, error)

	EventHead(ctx context.Context, p Principal, in protocol.EventHeadPayload) (protocol.EventHeadResultPayload, error)
	AuditTail(ctx context.Context, p Principal, in protocol.AuditTailPayload) (protocol.AuditTailResultPayload, error)
	ProcList(ctx context.Context, p Principal, in protocol.ProcListPayload) (protocol.ProcListResultPayload, error)
	ProcStop(ctx context.Context, p Principal, in protocol.ProcActionPayload) (protocol.ProcActionResultPayload, error)
	ProcKill(ctx context.Context, p Principal, in protocol.ProcActionPayload) (protocol.ProcActionResultPayload, error)
	ProcClean(ctx context.Context, p Principal, in protocol.ProcActionPayload) (protocol.ProcActionResultPayload, error)
}
