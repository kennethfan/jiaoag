package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/memory"
	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const rememberVersion = "1"

// NewRemember 返回跨会话记忆的追加工具。
//
// 和写文件工具一样，Prepare 只产出变更提案：用户看到 diff 并批准后才落盘。
// 区别是它只追加一条格式化后的记忆，不让模型整文件覆盖。
func NewRemember(ws *workspace.Workspace) Tool { return &rememberTool{ws: ws} }

type rememberTool struct{ ws *workspace.Workspace }

type rememberArgs struct {
	Text string `json:"text"`
}

func (t *rememberTool) Spec() Spec {
	return Spec{
		Name: "remember",
		Description: "把值得跨会话保留的项目事实追加到工作区根目录的 " + memory.FileName + "。" +
			"适合：构建/测试命令、目录约定、踩过的坑、已定决策；不适合临时进度或会话历史。" +
			"调用不会立刻落盘：用户会先看到 diff 并决定是否批准。",
		Version: rememberVersion,
		Effects: []Effect{EffectProposeWrite},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "一条独立成立的事实，尽量一句话；超过 500 字会被拒绝"}
  },
  "required": ["text"]
}`),
	}
}

func (t *rememberTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	var args rememberArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	text := strings.Join(strings.Fields(args.Text), " ")
	if text == "" {
		return Prepared{}, invalidArgs(call.Name, "text 不能为空")
	}
	if utf8.RuneCountInString(text) > memory.MaxEntryRunes {
		return Prepared{}, invalidArgs(call.Name, "text 超过 %d 字上限，请拆成多条或精简", memory.MaxEntryRunes)
	}

	abs, err := t.ws.Resolve(memory.FileName)
	if err != nil {
		return Prepared{}, err
	}
	old, exists, err := t.ws.Snapshot(memory.FileName)
	if err != nil {
		return Prepared{}, err
	}
	if exists && bytes.Contains(old, []byte(text)) {
		return Prepared{}, invalidArgs(call.Name, "%s 里已经有一条相同内容", memory.FileName)
	}

	entry := memory.FormatEntry(text, time.Now())
	newContent := bytes.TrimRight(old, "\n")
	if len(newContent) > 0 {
		newContent = append(newContent, '\n')
	}
	newContent = append(newContent, entry...)
	newContent = append(newContent, '\n')
	if exists {
		newContent = patch.Normalize(newContent, patch.LineEndings(old))
	}
	if len(newContent) > memory.MaxLoadBytes {
		return Prepared{}, invalidArgs(call.Name,
			"追加后 %s 会超过 %d 字节上限；请先删除或精简旧条目", memory.FileName, memory.MaxLoadBytes)
	}

	change, err := buildChange(call.Name, memory.FileName, memory.FileName, "", old, exists, newContent)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Call: call, Spec: t.Spec(), Args: args, Paths: []string{abs}, Change: change}, nil
}

func (t *rememberTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Content: "remember 必须经过审批：落盘由上层在批准后调用 workspace.Write 完成", IsError: true}, nil
}
