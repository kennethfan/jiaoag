package tools

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// truncateOutput 把超过 MaxOutputBytes 的输出截短，并把截断事实回传。
//
// 按 UTF-8 字符边界回退：工具输出里混着中文时，按字节硬切会留下半个字符，
// 模型和终端都会看到乱码。
func truncateOutput(s string) (string, bool) {
	if len(s) <= MaxOutputBytes {
		return s, false
	}
	cut := MaxOutputBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[已截断：原始输出 %d 字节，只保留前 %d 字节。缩小 path/glob 或 max_results]",
		len(s), cut), true
}

// capResult 是所有工具结果的统一出口：截断只在这里做一次，避免每个工具
// 各写一份上限、彼此漂移。
func capResult(res Result) Result {
	content, truncated := truncateOutput(res.Content)
	res.Content = content
	res.Truncated = res.Truncated || truncated
	return res
}

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
		return capResult(Result{
			Content: fmt.Sprintf("未知工具 %q；可用工具：%s", call.Name, strings.Join(r.Names(), ", ")),
			IsError: true,
		}), Prepared{}, nil
	}

	p, err := tool.Prepare(ctx, call)
	if err != nil {
		res, rerr := toolFailure(ctx, err)
		return capResult(res), Prepared{Call: call, Spec: tool.Spec()}, rerr
	}
	res, err := tool.Execute(ctx, p)
	if err != nil {
		res, rerr := toolFailure(ctx, err)
		return capResult(res), p, rerr
	}
	return capResult(res), p, nil
}
