package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	readOutputVersion = "1"

	// 与 workspace.MaxReadLines / MaxReadBytes 对齐：补看中段时不该出现
	// 一条工具调用就能把上下文撑爆的路径。
	maxOutputLines = 2000
	maxOutputBytes = 256 << 10
)

// NewReadOutput 返回按行补看命令全文日志的工具。
//
// 路径不来自模型：只接受 tool_call_id，经 app 反查 tool_calls.output_path，
// 再校验它落在输出目录之内。随机十六进制标识也不可枚举，因此读不到别的会话。
func NewReadOutput(env RuntimeEnv) Tool { return &readOutputTool{env: env} }

type readOutputTool struct {
	env RuntimeEnv
}

type readOutputArgs struct {
	ToolCallID string `json:"tool_call_id"`
	Offset     int    `json:"offset,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

func (t *readOutputTool) Spec() Spec {
	return Spec{
		Name: "read_output",
		Description: "读取某次命令调用的完整输出日志（按行、带行号）。run_command 的结果被截断时，" +
			"用它补看中段，不必重跑命令。",
		Version: readOutputVersion,
		Effects: []Effect{EffectRead},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool_call_id": {"type": "string",  "description": "命令工具结果的 tool_call_id"},
    "offset":       {"type": "integer", "description": "起始行号，从 1 开始"},
    "limit":        {"type": "integer", "description": "最多返回多少行，默认 500，上限 2000"}
  },
  "required": ["tool_call_id"]
}`),
	}
}

func (t *readOutputTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args readOutputArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if strings.TrimSpace(args.ToolCallID) == "" {
		return Prepared{}, invalidArgs(call.Name, "tool_call_id 不能为空")
	}
	if args.Offset < 0 {
		return Prepared{}, invalidArgs(call.Name, "offset 不能为负数")
	}
	if args.Limit < 0 {
		return Prepared{}, invalidArgs(call.Name, "limit 不能为负数")
	}
	return Prepared{Call: call, Spec: t.Spec(), Args: args}, nil
}

func (t *readOutputTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	args, ok := p.Args.(readOutputArgs)
	if !ok {
		return Result{}, fmt.Errorf("tools: read_output 参数类型异常")
	}
	if t.env.OutputPath == nil {
		return Result{}, fmt.Errorf("tools: read_output 没有输出查询能力")
	}
	path, err := t.env.OutputPath(ctx, args.ToolCallID)
	if err != nil {
		return Result{}, fmt.Errorf("tools: 读取 %s 的输出路径: %w", args.ToolCallID, err)
	}
	if err := t.checkBoundary(path); err != nil {
		return Result{}, err
	}

	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > maxOutputLines {
		limit = maxOutputLines
	}

	body, total, truncated, err := readLogFile(path, offset, limit)
	if err != nil {
		return Result{}, err
	}
	if body == "" {
		if total >= offset {
			// 单行超过 256 KB：不是 offset 越界，别给模型一个错误的修正方向。
			return Result{
				Content:   fmt.Sprintf("日志共 %d 行；第 %d 行超过 %d 字节上限，无法显示", total, offset, maxOutputBytes),
				Truncated: true,
			}, nil
		}
		return Result{
			Content:   fmt.Sprintf("日志共 %d 行；offset %d 超出末尾", total, offset),
			Truncated: truncated,
		}, nil
	}
	header := fmt.Sprintf("日志共 %d 行，显示 %d-%d", total, offset, offset+strings.Count(body, "\n")-1)
	if truncated {
		header += "（已截断，可继续用 offset 补看）"
	}
	return Result{Content: header + "\n" + body, Truncated: truncated}, nil
}

// checkBoundary 确认日志路径落在**本会话**的输出目录内。反查已经保证路径
// 来自我们自己的写入，但数据库被改过或记录跨会话泄漏时，这一层仍然要挡住。
func (t *readOutputTool) checkBoundary(path string) error {
	if t.env.OutputDir == "" {
		return fmt.Errorf("tools: 未配置 storage.output_dir")
	}
	if t.env.SessionID == "" {
		return fmt.Errorf("tools: read_output 未绑定会话，拒绝读取")
	}
	root, err := filepath.Abs(filepath.Join(t.env.OutputDir, t.env.SessionID))
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || escapesDir(rel) {
		return fmt.Errorf("tools: 输出文件 %s 不在输出目录内", path)
	}
	// 词法通过后，存在的路径再做一次软链解析检查。
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil
	}
	rel, err = filepath.Rel(realRoot, realPath)
	if err != nil || escapesDir(rel) {
		return fmt.Errorf("tools: 输出文件 %s 指向输出目录之外", path)
	}
	return nil
}

func escapesDir(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// readLogFile 按行读取 [offset, offset+limit)，返回带行号的正文。
func readLogFile(path string, offset, limit int) (body string, total int, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false, fmt.Errorf("tools: 打开输出文件: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var b strings.Builder
	bytes := 0
	lineNo := 0
	end := offset + limit - 1
	eof := false
	for lineNo < end {
		line, rerr := r.ReadString('\n')
		if line != "" {
			lineNo++
			if lineNo >= offset {
				entry := fmt.Sprintf("%d: %s", lineNo, strings.TrimRight(line, "\r\n"))
				if bytes+len(entry)+1 > maxOutputBytes {
					truncated = true
					break
				}
				b.WriteString(entry)
				b.WriteByte('\n')
				bytes += len(entry) + 1
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				eof = true
				break
			}
			return "", 0, false, fmt.Errorf("tools: 读取输出文件: %w", rerr)
		}
	}
	if !eof && !truncated {
		if _, perr := r.Peek(1); perr == nil {
			truncated = true
		}
	}
	// total 至少要能说清「一共有多少行」：继续读到 EOF 只为计数，
	// 不再累积正文，内存只保留本次返回的一段。
	for !eof {
		line, rerr := r.ReadString('\n')
		if line != "" {
			lineNo++
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", 0, false, fmt.Errorf("tools: 读取输出文件: %w", rerr)
		}
	}
	return b.String(), lineNo, truncated, nil
}
