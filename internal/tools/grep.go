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
	"regexp"
	"strings"

	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	grepVersion      = "1"
	grepDefaultMax   = 200
	grepMaxResults   = 1000
	grepMaxFileBytes = 1 << 20
	grepMaxLineBytes = 1 << 20
)

// errEnoughResults 用于提前结束遍历，不是真实错误。
var errEnoughResults = errors.New("tools: 结果已满")

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Glob            string `json:"glob,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	MaxResults      int    `json:"max_results,omitempty"`
}

type grepState struct {
	args grepArgs
	re   *regexp.Regexp
	root string
	max  int
}

// NewGrep 返回正则搜索工具。用 Go 的 RE2，不依赖外部 ripgrep。
func NewGrep(ws *workspace.Workspace) Tool { return &grepTool{ws: ws} }

type grepTool struct {
	ws *workspace.Workspace
}

func (t *grepTool) Spec() Spec {
	return Spec{
		Name:        "grep",
		Description: "按正则表达式递归搜索工作区内的文本文件，返回「路径:行号:内容」。自动跳过 .git、node_modules、vendor 与二进制文件。",
		Version:     grepVersion,
		Effects:     []Effect{EffectRead},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern":          {"type": "string",  "description": "Go 正则表达式"},
    "path":             {"type": "string",  "description": "搜索的起始相对路径，默认工作区根"},
    "glob":             {"type": "string",  "description": "只看匹配该 glob 的文件，例如 **/*.go"},
    "case_insensitive": {"type": "boolean", "description": "是否忽略大小写"},
    "max_results":      {"type": "integer", "description": "最多返回多少条匹配，默认 200"}
  },
  "required": ["pattern"]
}`),
	}
}

func (t *grepTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args grepArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return Prepared{}, invalidArgs(call.Name, "pattern 不能为空")
	}
	if args.MaxResults < 0 {
		return Prepared{}, invalidArgs(call.Name, "max_results 不能为负数")
	}
	if args.Path == "" {
		args.Path = "."
	}
	if args.Glob != "" {
		if err := Validate(args.Glob); err != nil {
			return Prepared{}, invalidArgs(call.Name, "%v", err)
		}
	}
	expr := args.Pattern
	if args.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Prepared{}, invalidArgs(call.Name, "正则表达式无效：%v", err)
	}
	abs, err := t.ws.Resolve(args.Path)
	if err != nil {
		return Prepared{}, err
	}
	max := args.MaxResults
	if max <= 0 {
		max = grepDefaultMax
	}
	if max > grepMaxResults {
		max = grepMaxResults
	}
	return Prepared{
		Call:  call,
		Spec:  t.Spec(),
		Args:  grepState{args: args, re: re, root: t.ws.Root(), max: max},
		Paths: []string{abs},
	}, nil
}

func (t *grepTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	st, ok := p.Args.(grepState)
	if !ok {
		return Result{}, fmt.Errorf("tools: grep 参数类型异常")
	}

	var (
		matches   []string
		truncated bool
	)
	err := t.ws.Walk(st.args.Path, func(rel string, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size > grepMaxFileBytes {
			return nil
		}
		if st.args.Glob != "" && !Match(st.args.Glob, rel) {
			return nil
		}
		remaining := st.max - len(matches)
		// 多要一条：拿满 remaining 无法区分「恰好到头」与「还有更多」。
		found, err := searchFile(filepath.Join(st.root, filepath.FromSlash(rel)), rel, st.re, remaining+1)
		if err != nil {
			// 单个文件读不了不终止整次搜索：权限、竞态删除都属正常。
			return nil
		}
		matches = append(matches, found...)
		if len(matches) > st.max {
			matches = matches[:st.max]
			truncated = true
			return errEnoughResults
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEnoughResults) {
		return toolFailure(ctx, err)
	}
	if len(matches) == 0 {
		return Result{Content: "没有匹配"}, nil
	}

	var b strings.Builder
	for _, m := range matches {
		b.WriteString(m)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "[已截断：达到上限 %d 条。缩小 path/glob 或调大 max_results]\n", st.max)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}

// searchFile 返回该文件里最多 limit 条匹配，格式为「相对路径:行号:内容」。
func searchFile(abs, rel string, re *regexp.Regexp, limit int) ([]string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	probe := make([]byte, 8<<10)
	n, _ := f.Read(probe)
	if containsNUL(probe[:n]) {
		return nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), grepMaxLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if !re.MatchString(text) {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d:%s", rel, line, text))
		if len(out) >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
