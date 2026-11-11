package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	globVersion    = "1"
	globDefaultMax = 200
	globMaxResults = 1000

	// globCollectLimit 是遍历时的收集上限：模式写得太宽时先在内存上兜住。
	globCollectLimit = 50000
)

// errCollectLimit 用于提前结束遍历，不是真实错误。
var errCollectLimit = errors.New("tools: 已达收集上限")

type globArgs struct {
	Pattern    string `json:"pattern"`
	MaxResults int    `json:"max_results,omitempty"`
}

type globState struct {
	pattern string
	max     int
}

// NewGlob 返回按模式列文件的工具，支持 `**`。
func NewGlob(ws *workspace.Workspace) Tool { return &globTool{ws: ws} }

type globTool struct {
	ws *workspace.Workspace
}

func (t *globTool) Spec() Spec {
	return Spec{
		Name:        "glob",
		Description: "按 glob 模式列出工作区内的文件（支持 ** 跨目录）。只看文件，不返回目录。",
		Version:     globVersion,
		Effects:     []Effect{EffectRead},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern":     {"type": "string",  "description": "glob 模式，例如 **/*.go 或 internal/**/testdata/*.json"},
    "max_results": {"type": "integer", "description": "最多返回多少条路径，默认 200"}
  },
  "required": ["pattern"]
}`),
	}
}

func (t *globTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args globArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return Prepared{}, invalidArgs(call.Name, "pattern 不能为空")
	}
	// 模式非法要在 Prepare 阶段就报出来：否则模型看到的是「没有匹配的文件」，
	// 会以为是路径问题而不是自己的模式写错了。
	if err := Validate(args.Pattern); err != nil {
		return Prepared{}, invalidArgs(call.Name, "%v", err)
	}
	if args.MaxResults < 0 {
		return Prepared{}, invalidArgs(call.Name, "max_results 不能为负数")
	}
	max := args.MaxResults
	if max <= 0 {
		max = globDefaultMax
	}
	if max > globMaxResults {
		max = globMaxResults
	}
	return Prepared{
		Call: call,
		Spec: t.Spec(),
		Args: globState{pattern: args.Pattern, max: max},
	}, nil
}

func (t *globTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	st, ok := p.Args.(globState)
	if !ok {
		return Result{}, fmt.Errorf("tools: glob 参数类型异常")
	}

	var (
		found   []string
		overCap bool
	)
	err := t.ws.Walk(".", func(rel string, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if Match(st.pattern, rel) {
			// 先判上限再收：正好命中 globCollectLimit 条且没有更多时不该报截断。
			if len(found) >= globCollectLimit {
				overCap = true
				return errCollectLimit
			}
			found = append(found, rel)
		}
		return nil
	})
	if err != nil && !errors.Is(err, errCollectLimit) && ctx.Err() == nil {
		return toolFailure(ctx, err)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if len(found) == 0 {
		return Result{Content: "没有匹配的文件"}, nil
	}

	sort.Strings(found)
	truncated := overCap
	if len(found) > st.max {
		found = found[:st.max]
		truncated = true
	}
	var b strings.Builder
	for _, rel := range found {
		b.WriteString(rel)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "[已截断：只显示前 %d 条。收窄 pattern 或调大 max_results]\n", st.max)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}
