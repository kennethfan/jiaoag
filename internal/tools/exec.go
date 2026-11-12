package tools

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrUnknownTool 表示注册表里没有这个名字。它降级成工具错误回给模型，
// 而不是让任务失败：模型看到可用工具列表后通常能自己纠正。
type ErrUnknownTool struct {
	Name      string
	Available []string
}

func (e *ErrUnknownTool) Error() string {
	return fmt.Sprintf("未知工具 %q；可用工具：%s", e.Name, strings.Join(e.Available, ", "))
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

// Failure 是对外的 toolFailure：已经应用过输出上限，可以直接回给模型。
func Failure(ctx context.Context, err error) (Result, error) {
	res, rerr := toolFailure(ctx, err)
	return capResult(res), rerr
}

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

// Prepare 解析参数、解析路径，不产生任何副作用。
//
// 与 Run 分开是为了给写入工具留审批插口：准备与执行之间要插一次用户裁决
// （ADR-0006）。未知工具也以错误返回，由调用方用 Failure 降级。
func (r *Registry) Prepare(ctx context.Context, call Call) (Prepared, error) {
	tool, ok := r.Get(call.Name)
	if !ok {
		return Prepared{}, &ErrUnknownTool{Name: call.Name, Available: r.Names()}
	}
	p, err := tool.Prepare(ctx, call)
	if err != nil {
		// 失败时也带上 Spec 与 Call：调用方仍要记录 tool.prepared，
		// 才能看出「它想调什么、想改哪些文件」。
		return Prepared{Call: call, Spec: tool.Spec()}, err
	}
	return p, nil
}

// Execute 执行已准备的调用，并把输出收敛到 MaxOutputBytes。
func (r *Registry) Execute(ctx context.Context, p Prepared) (Result, error) {
	tool, ok := r.Get(p.Call.Name)
	if !ok {
		res, rerr := Failure(ctx, &ErrUnknownTool{Name: p.Call.Name, Available: r.Names()})
		return res, rerr
	}
	res, err := tool.Execute(ctx, p)
	if err != nil {
		return Failure(ctx, err)
	}
	return capResult(res), nil
}

// Run 是「准备 + 执行」的组合，供不需要审批的只读工具与测试使用。
//
// 返回的 Prepared 用来记录 tool.prepared 事件（路径、副作用）。
func (r *Registry) Run(ctx context.Context, call Call) (Result, Prepared, error) {
	p, err := r.Prepare(ctx, call)
	if err != nil {
		res, rerr := Failure(ctx, err)
		return res, p, rerr
	}
	res, err := r.Execute(ctx, p)
	return res, p, err
}
