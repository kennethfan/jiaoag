package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kennethfan/jiaoag/internal/tools"
)

// remoteTool 是一个 MCP 工具的宿主侧适配器。
//
// 它只做三件事：校验参数、把调用标记成 External、把结果翻译成 tools.Result。
// 权限判定、审批、审计全部由宿主既有链路完成——适配器不能绕过它们。
type remoteTool struct {
	session    *sdk.ClientSession
	remoteName string
	spec       tools.Spec
	source     string
	trust      bool
	timeout    time.Duration
	log        *slog.Logger
}

func (t *remoteTool) Spec() tools.Spec { return t.spec }

func (t *remoteTool) Prepare(ctx context.Context, call tools.Call) (tools.Prepared, error) {
	args, err := decodeArgs(call.Args)
	if err != nil {
		return tools.Prepared{}, fmt.Errorf("%s: %w", t.spec.Name, err)
	}
	if err := checkRequired(t.spec.Schema, args); err != nil {
		return tools.Prepared{}, fmt.Errorf("%s: %w", t.spec.Name, err)
	}
	return tools.Prepared{
		Call: call,
		Spec: t.spec,
		Args: args,
		External: &tools.External{
			Source:  t.source,
			Trusted: t.trust,
		},
	}, nil
}

func (t *remoteTool) Execute(ctx context.Context, prepared tools.Prepared) (tools.Result, error) {
	args, _ := prepared.Args.(map[string]any)
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	start := time.Now()
	res, err := t.session.CallTool(callCtx, &sdk.CallToolParams{
		Name:      t.remoteName,
		Arguments: args,
	})
	elapsed := time.Since(start)
	if err != nil {
		// 任务取消必须原样上抛，agent 靠它把调用收尾成 canceled。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tools.Result{}, ctxErr
		}
		msg := fmt.Sprintf("%s 调用失败：%v", t.spec.Name, err)
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("%s 调用超时（%s）", t.spec.Name, t.timeout)
		}
		return tools.Result{Content: msg, IsError: true, Elapsed: elapsed}, nil
	}

	content, truncated := renderResult(res)
	return tools.Result{
		Content:   content,
		IsError:   res.IsError,
		Truncated: truncated,
		Elapsed:   elapsed,
	}, nil
}

// decodeArgs 只接受 JSON object。MCP 的 inputSchema 顶层也是 object，
// 传数组/标量说明模型或调用方理解错了，早失败比发给 server 更清楚。
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return nil, fmt.Errorf("参数必须是 JSON object：%w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// checkRequired 是最小校验：schema 里 required 列出的键必须存在。
// 完整 JSON Schema 校验交给 server——它是参数的最终解释者，宿主重复实现
// 只会引入两边不一致。
func checkRequired(schema json.RawMessage, args map[string]any) error {
	var s struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Required) == 0 {
		return nil
	}
	var missing []string
	for _, name := range s.Required {
		if _, ok := args[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("缺少必填参数：%s", strings.Join(missing, ", "))
}

// renderResult 把 MCP 内容块翻译成模型可读文本。
//
// v2.0 只取文本：图片/音频/资源不是当前协议能呈现的东西，用占位符说明
// 「有内容但没展示」，比静默丢弃更容易排查。
func renderResult(res *sdk.CallToolResult) (string, bool) {
	var b strings.Builder
	add := func(text string) {
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			add(v.Text)
		case *sdk.ImageContent:
			add(fmt.Sprintf("[图片 %s，%d 字节，未展示]", v.MIMEType, len(v.Data)))
		case *sdk.AudioContent:
			add(fmt.Sprintf("[音频 %s，%d 字节，未展示]", v.MIMEType, len(v.Data)))
		case *sdk.EmbeddedResource:
			if v.Resource == nil {
				add("[内嵌资源为空]")
				break
			}
			if v.Resource.Text != "" {
				add(v.Resource.Text)
				break
			}
			add(fmt.Sprintf("[内嵌资源 %s（%s），%d 字节，未展示]",
				v.Resource.URI, v.Resource.MIMEType, len(v.Resource.Blob)))
		default:
			add(fmt.Sprintf("[不支持的内容类型 %T，未展示]", c))
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			add(string(raw))
		}
	}
	if b.Len() == 0 {
		add("（工具没有返回内容）")
	}
	return truncateResult(b.String())
}

func truncateResult(s string) (string, bool) {
	if len(s) <= tools.MaxOutputBytes {
		return s, false
	}
	// 外部工具的输出不可控：保留头尾，中间截断，既能看到开头也能看到结论。
	head := tools.MaxOutputBytes * 3 / 4
	tail := tools.MaxOutputBytes - head
	return s[:head] +
		fmt.Sprintf("\n…（输出已截断，原长 %d 字节）…\n", len(s)) +
		s[len(s)-tail:], true
}
