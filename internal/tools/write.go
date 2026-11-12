package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	writeFileVersion = "1"
	editFileVersion  = "1"

	// maxEditBytes 是 edit_file 能处理的文件上限。超过它时整文件读入再替换
	// 会把内存和 diff 都撑爆，交给 write_file 更诚实。
	maxEditBytes = 1 << 20
)

// NewWriteFile 返回整文件覆盖工具。它不直接落盘：Prepare 只产出变更提案，
// 审批通过后由 agent 调 workspace.Write。
func NewWriteFile(ws *workspace.Workspace) Tool { return &writeFileTool{ws: ws} }

// NewEditFile 返回按片段替换的工具，同样只产出提案。
func NewEditFile(ws *workspace.Workspace) Tool { return &editFileTool{ws: ws} }

type writeFileTool struct{ ws *workspace.Workspace }

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *writeFileTool) Spec() Spec {
	return Spec{
		Name: "write_file",
		Description: "写入整个文件（覆盖已有内容，文件不存在则新建）。" +
			"调用不会立刻落盘：用户会先看到 diff 并决定是否批准。",
		Version: writeFileVersion,
		Effects: []Effect{EffectProposeWrite},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string", "description": "相对工作区根的路径，例如 internal/app/app.go"},
    "content": {"type": "string", "description": "完整的新文件内容；传空串表示清空文件"}
  },
  "required": ["path", "content"]
}`),
	}
}

func (t *writeFileTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args writeFileArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return Prepared{}, invalidArgs(call.Name, "path 不能为空")
	}
	abs, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Prepared{}, err
	}
	old, exists, err := t.ws.Snapshot(args.Path)
	if err != nil {
		return Prepared{}, err
	}
	newContent := []byte(args.Content)
	if exists {
		// 保留原文件的换行风格：把一个 CRLF 文件整体改成 LF 会污染整篇 diff。
		newContent = patch.Normalize(newContent, patch.LineEndings(old))
	}
	change, err := buildChange(call.Name, args.Path, old, exists, newContent)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Call: call, Spec: t.Spec(), Args: args, Paths: []string{abs}, Change: change}, nil
}

func (t *writeFileTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Content: "write_file 必须经过审批：落盘由上层在批准后调用 workspace.Write 完成", IsError: true}, nil
}

type editFileTool struct{ ws *workspace.Workspace }

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func (t *editFileTool) Spec() Spec {
	return Spec{
		Name: "edit_file",
		Description: "把文件里的一段文本替换成另一段（old_string 必须唯一命中，除非 replace_all=true）。" +
			"调用不会立刻落盘：用户会先看到 diff 并决定是否批准。",
		Version: editFileVersion,
		Effects: []Effect{EffectProposeWrite},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":        {"type": "string",  "description": "相对工作区根的路径"},
    "old_string":  {"type": "string",  "description": "要被替换的原文；默认必须在文件里唯一出现"},
    "new_string":  {"type": "string",  "description": "替换后的文本；空串表示删除这段"},
    "replace_all": {"type": "boolean", "description": "为 true 时替换全部命中"}
  },
  "required": ["path", "old_string", "new_string"]
}`),
	}
}

func (t *editFileTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args editFileArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return Prepared{}, invalidArgs(call.Name, "path 不能为空")
	}
	if args.OldString == "" {
		return Prepared{}, invalidArgs(call.Name, "old_string 不能为空；要整文件覆盖请用 write_file")
	}
	abs, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Prepared{}, err
	}
	old, exists, err := t.ws.Snapshot(args.Path)
	if err != nil {
		return Prepared{}, err
	}
	if !exists {
		return Prepared{}, invalidArgs(call.Name, "文件 %s 不存在；新建文件请用 write_file", args.Path)
	}
	if len(old) > maxEditBytes {
		return Prepared{}, invalidArgs(call.Name,
			"文件 %s 有 %d 字节，超过 edit_file 的 %d 字节上限；请用 write_file", args.Path, len(old), maxEditBytes)
	}

	endings := patch.LineEndings(old)
	text := string(patch.ToLF(old))
	// 模型给的多半是 LF；先把 needle 归一，否则在 CRLF 文件里永远匹配不上。
	needle := string(patch.ToLF([]byte(args.OldString)))
	repl := string(patch.ToLF([]byte(args.NewString)))

	hits := strings.Count(text, needle)
	switch {
	case hits == 0:
		return Prepared{}, invalidArgs(call.Name, "在 %s 里没有找到 old_string；请先 read_file 确认原文", args.Path)
	case hits > 1 && !args.ReplaceAll:
		// 唯一性检查是「改对地方」的唯一保障：多处命中时随便挑一处改，
		// 用户很难从 diff 里看出改的是不是他要的那处。
		return Prepared{}, invalidArgs(call.Name,
			"old_string 在 %s 里出现了 %d 次；请给更长的上下文让它唯一，或设置 replace_all=true", args.Path, hits)
	}

	limit := 1
	if args.ReplaceAll {
		limit = -1
	}
	updated := strings.Replace(text, needle, repl, limit)
	newContent := patch.Normalize([]byte(updated), endings)

	change, err := buildChange(call.Name, args.Path, old, exists, newContent)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Call: call, Spec: t.Spec(), Args: args, Paths: []string{abs}, Change: change}, nil
}

func (t *editFileTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Content: "edit_file 必须经过审批：落盘由上层在批准后调用 workspace.Write 完成", IsError: true}, nil
}

// buildChange 是两种写入工具共用的提案构造：没有变化时直接报错，
// 避免让用户去批准一份空 diff。
func buildChange(tool, rel string, old []byte, exists bool, newContent []byte) (*Change, error) {
	if exists && bytes.Equal(old, newContent) {
		return nil, invalidArgs(tool, "%s 的内容没有变化，不需要写入", rel)
	}
	diff, truncated, err := patch.Unified(rel, old, newContent, exists)
	if err != nil {
		return nil, err
	}
	// 行级 diff 表达不了的改动不能进审批：用户看不见，就无从批准。
	// 只有两种情况会得到空 diff——新建空文件，以及只在结尾换行上不同。
	if diff == "" {
		if !exists && len(newContent) == 0 {
			return nil, invalidArgs(tool, "新建空文件没有可审内容；如确实需要空文件，请让用户手工创建")
		}
		return nil, invalidArgs(tool, "%s 与原文只差结尾换行，行级 diff 无法展示这种改动", rel)
	}
	change := &Change{
		Path:       rel,
		Exists:     exists,
		NewHash:    patch.Hash(newContent),
		NewContent: newContent,
		Diff:       diff,
		Truncated:  truncated,
	}
	if exists {
		change.BaseHash = patch.Hash(old)
	}
	return change, nil
}
