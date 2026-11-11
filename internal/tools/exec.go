package tools

import (
	"context"
	"fmt"
	"strings"
)

// toolFailure 把任意错误降级成工具错误结果。
//
// 只有 ctx 取消会向上抛：其余情况（路径越界、二进制、参数错）都是模型可以
// 自己修正的问题，交回模型比让整个任务失败更有用。
func toolFailure(ctx context.Context, err error) (Result, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, ctxErr
	}
	return Result{Content: err.Error(), IsError: true}, nil
}

// Run 是「准备 + 执行」的组合，供 agent 直接调用。
//
// 返回的 Prepared 用来记录 tool.prepared 事件（路径、副作用），未知工具
// name 直接以工具错误返回，不中断任务。
func (r *Registry) Run(ctx context.Context, call Call) (Result, Prepared, error) {
	tool, ok := r.Get(call.Name)
	if !ok {
		return Result{
			Content: fmt.Sprintf("未知工具 %q；可用工具：%s", call.Name, strings.Join(r.Names(), ", ")),
			IsError: true,
		}, Prepared{}, nil
	}

	p, err := tool.Prepare(ctx, call)
	if err != nil {
		res, rerr := toolFailure(ctx, err)
		return res, Prepared{Call: call, Spec: tool.Spec()}, rerr
	}
	res, err := tool.Execute(ctx, p)
	if err != nil {
		res, rerr := toolFailure(ctx, err)
		return res, p, rerr
	}
	return res, p, nil
}
