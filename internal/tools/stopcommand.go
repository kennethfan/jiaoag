package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const stopCommandVersion = "1"

// NewStopCommand 返回停止后台进程的工具。
//
// 只作用于本会话自己启动的进程：id 必须能在会话的 ProcTable 里查到，
// 模型给不了任意 pid，也碰不到别的会话。因此它被声明为只读副作用——
// 停掉自己起的进程不需要用户再批一次。
func NewStopCommand(env RuntimeEnv) Tool { return &stopCommandTool{env: env} }

type stopCommandTool struct {
	env RuntimeEnv
}

type stopCommandArgs struct {
	ID string `json:"id"`
}

func (t *stopCommandTool) Spec() Spec {
	return Spec{
		Name: "stop_command",
		Description: "停止一个由 run_command background=true 启动的后台进程（整进程组/整 Job 一起终止）。" +
			"id 就是那次启动返回的 tool_call_id。",
		Version: stopCommandVersion,
		Effects: []Effect{EffectRead},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "后台启动时返回的 tool_call_id"}
  },
  "required": ["id"]
}`),
	}
}

func (t *stopCommandTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	var args stopCommandArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	args.ID = strings.TrimSpace(args.ID)
	if args.ID == "" {
		return Prepared{}, invalidArgs(call.Name, "id 不能为空")
	}
	return Prepared{Call: call, Spec: t.Spec(), Args: args}, nil
}

func (t *stopCommandTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	args, ok := p.Args.(stopCommandArgs)
	if !ok {
		return Result{}, fmt.Errorf("tools: stop_command 参数不可用")
	}
	if t.env.Procs == nil {
		return Result{Content: "宿主未启用后台命令，没有可停止的进程", IsError: true}, nil
	}
	found, err := t.env.Procs.Stop(t.env.SessionID, args.ID)
	if err != nil {
		return Result{Content: "停止后台进程失败：" + err.Error(), IsError: true}, nil
	}
	if !found {
		return Result{
			Content: "没有找到这个后台进程（" + args.ID + "）；可能已经退出，或不属于本会话",
			IsError: true,
		}, nil
	}
	return Result{Content: "已停止后台进程 " + args.ID}, nil
}
