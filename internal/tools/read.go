package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kennethfan/jiaoag/internal/workspace"
)

const readFileVersion = "1"

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`

	// ws 与 label 由 Prepare 填入：路径可能带 alias: 前缀（只读附加根）。
	target *workspace.Workspace
	label  string
}

// NewReadFile 返回带行号的文本读取工具。
func NewReadFile(ws *workspace.Workspace) Tool {
	return &readFileTool{ws: ws}
}

// NewReadFileWithRoots 允许用 alias:path 读取会话挂的附加根（ADR-0036）。
func NewReadFileWithRoots(ws *workspace.Workspace, roots *workspace.Roots) Tool {
	return &readFileTool{ws: ws, roots: roots}
}

type readFileTool struct {
	ws    *workspace.Workspace
	roots *workspace.Roots
}

func (t *readFileTool) Spec() Spec {
	return Spec{
		Name:        "read_file",
		Description: "读取工作区内的文本文件并返回带行号的内容。大文件可用 offset/limit 分页。附加根用 alias:path 形式（只读）。",
		Version:     readFileVersion,
		Effects:     []Effect{EffectRead},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":   {"type": "string",  "description": "相对工作区根的路径，例如 internal/app/app.go"},
    "offset": {"type": "integer", "description": "起始行号，从 1 开始"},
    "limit":  {"type": "integer", "description": "最多返回多少行，默认 2000"}
  },
  "required": ["path"]
}`),
	}
}

func (t *readFileTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args readFileArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return Prepared{}, invalidArgs(call.Name, "path 不能为空")
	}
	if args.Offset < 0 {
		return Prepared{}, invalidArgs(call.Name, "offset 不能为负数")
	}
	if args.Limit < 0 {
		return Prepared{}, invalidArgs(call.Name, "limit 不能为负数")
	}
	target, rel, label, err := t.resolve(args.Path)
	if err != nil {
		return Prepared{}, invalidArgs(call.Name, "%v", err)
	}
	abs, err := target.Resolve(rel)
	if err != nil {
		return Prepared{}, err
	}
	args.Path = rel
	args.target = target
	args.label = label
	return Prepared{
		Call:  call,
		Spec:  t.Spec(),
		Args:  args,
		Paths: []string{abs},
	}, nil
}

// resolve 解析 alias: 前缀；没挂载附加根时退化成主根。
func (t *readFileTool) resolve(path string) (*workspace.Workspace, string, string, error) {
	if t.roots == nil {
		return t.ws, path, "", nil
	}
	return t.roots.Resolve(path)
}

func (t *readFileTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	args, ok := p.Args.(readFileArgs)
	if !ok {
		return Result{}, fmt.Errorf("tools: read_file 参数类型异常")
	}
	target := args.target
	if target == nil {
		target = t.ws
	}
	fc, err := target.Read(args.Path, args.Offset, args.Limit)
	if err != nil {
		return toolFailure(ctx, err)
	}

	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	display := fc.Path
	if args.label != "" {
		display = args.label + ":" + fc.Path
	}
	var b strings.Builder
	if len(fc.Lines) == 0 {
		fmt.Fprintf(&b, "（%s 没有内容或起始行 %d 超出末尾）\n", display, offset)
	}
	for i, line := range fc.Lines {
		fmt.Fprintf(&b, "%d: %s\n", offset+i, line)
	}
	if fc.Truncated {
		last := offset + len(fc.Lines) - 1
		fmt.Fprintf(&b, "\n[已截断：共 %d 行，本次显示 %d-%d。用 offset/limit 继续读取]\n", fc.TotalLines, offset, last)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}
