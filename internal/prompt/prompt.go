// Package prompt 负责系统提示词的分层拼装与指纹。
//
// M3 只有 builtin 一层：核心规则写死在下面，工作区/系统等易变信息用变量
// 注入。变量走白名单，拼错或拼了未定义的变量直接报错——提示词静默丢一段
// 比报错更难发现。项目级覆盖留到 M4。
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Layer 是提示词的一段来源。Source 进日志，用于回答「这段是谁给的」。
type Layer struct {
	Source string // 例如 builtin:core
	Text   string
}

// Result 是拼装结果。Hash 用于冻结提示词版本，不含易变变量（日期）。
type Result struct {
	Text string
	Hash string
}

var tokenPattern = regexp.MustCompile(`\{\{([a-z][a-z0-9_]*)\}\}`)

// AllowedVars 是允许注入的变量名。新增变量必须同时改这里，避免模板里
// 出现无法解析的占位符却静默保留。
var AllowedVars = map[string]bool{
	"workspace": true,
	"os":        true,
	"shell":     true,
	"date":      true,
	"model":     true,
	"tools":     true,
}

// volatileVars 不进 Hash：同一天内跑两次任务应得到同一个提示词指纹。
var volatileVars = map[string]bool{"date": true}

// Builtin 返回内置核心层。
func Builtin() Layer {
	return Layer{Source: "builtin:core", Text: builtinCore}
}

const builtinCore = `你是 jiaoag 的编码助手，在用户的本地工作区里工作。

当前工作区：{{workspace}}
操作系统：{{os}}
默认 shell：{{shell}}
今天：{{date}}
模型：{{model}}

工作方式：
- 你对代码库的了解只能来自工具返回的内容。不确定就先 grep 定位，再用 read_file 读具体文件。
- 路径一律使用相对工作区根的路径，不要使用绝对路径。
- 引用代码时写清「相对路径:行号」，让用户能直接跳转。
- 工具返回错误时先读错误信息：参数不对就修正后重试，路径不存在就换目标。
- 回答使用简体中文。先说结论，再给必要细节，不要复述整个文件。
- 不要编造没有读过的文件内容或运行结果。

可用工具：
{{tools}}`

// Assemble 按顺序拼接各层，替换全部变量。
func Assemble(layers []Layer, vars map[string]string) (Result, error) {
	for name := range vars {
		if !AllowedVars[name] {
			return Result{}, fmt.Errorf("prompt: 未定义的变量 %q", name)
		}
	}
	var b strings.Builder
	for _, layer := range layers {
		text, err := substitute(layer.Text, vars)
		if err != nil {
			return Result{}, err
		}
		b.WriteString(strings.TrimRight(text, "\n"))
		b.WriteString("\n\n")
	}
	return Result{
		Text: strings.TrimRight(b.String(), "\n"),
		Hash: Hash(layers, vars),
	}, nil
}

// Hash 计算提示词指纹：层来源 + 文本 + 非易变变量。
func Hash(layers []Layer, vars map[string]string) string {
	var b strings.Builder
	for _, layer := range layers {
		fmt.Fprintf(&b, "layer:%s:%s\n", layer.Source, layer.Text)
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		if volatileVars[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "var:%s=%s\n", name, vars[name])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func substitute(text string, vars map[string]string) (string, error) {
	var err error
	out := tokenPattern.ReplaceAllStringFunc(text, func(token string) string {
		name := token[2 : len(token)-2]
		if !AllowedVars[name] {
			if err == nil {
				err = fmt.Errorf("prompt: 未定义的变量 %q", name)
			}
			return token
		}
		value, ok := vars[name]
		if !ok {
			if err == nil {
				err = fmt.Errorf("prompt: 变量 %q 没有提供值", name)
			}
			return token
		}
		return value
	})
	return out, err
}
