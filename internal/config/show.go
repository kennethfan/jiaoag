package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ShownField 是 config show 的一行：生效值 + 它从哪里来。
type ShownField struct {
	Path   string `json:"path"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Show 返回配置文件路径与全部生效字段。
//
// 来源只有三种：config.yaml:<行号>、默认值、$ENV 名（密钥类）。
// 密钥不打印明文，只报来源与解析后的长度——否则 config show 本身就成了泄漏面。
func Show(path string) (string, []ShownField, error) {
	if path == "" {
		path = DefaultPath()
	}
	p, err := ExpandHome(path)
	if err != nil {
		return "", nil, err
	}
	cfg, err := Load(p)
	if err != nil {
		return "", nil, err
	}
	lines := map[string]int{}
	if raw, rerr := os.ReadFile(p); rerr == nil {
		var doc yaml.Node
		if yaml.Unmarshal(raw, &doc) == nil {
			collectNodeLines(&doc, "", lines, 0)
		}
	}
	base := filepath.Base(p)
	fields := Fields(cfg)
	out := make([]ShownField, 0, len(fields))
	for _, f := range fields {
		src := "默认值"
		if line, ok := lines[f.Path]; ok {
			src = fmt.Sprintf("%s:%d", base, line)
		}
		out = append(out, ShownField{Path: f.Path, Value: formatField(f), Source: src})
	}
	// 计算出来的生效值：context_budget_tokens 为 0 时预算由 chars 折算，
	// 只看字段本身会以为「没设预算」。
	if cfg.Agent.ContextBudgetTokens == 0 && cfg.Agent.ContextBudgetChars > 0 {
		out = append(out, ShownField{
			Path:   "agent.context_budget_tokens（生效）",
			Value:  fmt.Sprint(cfg.Agent.ContextBudgetTokenValue()),
			Source: "由 context_budget_chars 折算",
		})
	}
	return p, out, nil
}

func collectNodeLines(n *yaml.Node, prefix string, out map[string]int, depth int) {
	if n == nil || depth > 12 {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			collectNodeLines(c, prefix, out, depth+1)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			p := joinPath(prefix, key.Value)
			out[p] = key.Line
			collectNodeLines(val, p, out, depth+1)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			p := fmt.Sprintf("%s[%d]", prefix, i)
			out[p] = c.Line
			collectNodeLines(c, p, out, depth+1)
		}
	}
}

func formatField(f Field) string {
	if f.Empty {
		return "（未设置）"
	}
	if f.Secret {
		return formatSecret(f)
	}
	switch f.Type.Kind() {
	case reflect.Slice:
		if f.Value.Len() == 0 {
			return "（空）"
		}
		parts := make([]string, 0, f.Value.Len())
		for i := 0; i < f.Value.Len() && i < 6; i++ {
			parts = append(parts, clipValue(fmt.Sprint(f.Value.Index(i).Interface())))
		}
		if f.Value.Len() > 6 {
			parts = append(parts, fmt.Sprintf("…（共 %d 项）", f.Value.Len()))
		}
		return strings.Join(parts, ", ")
	case reflect.String:
		s := fmt.Sprint(f.Value.Interface())
		if s == "" {
			return "（未设置）"
		}
		return clipValue(s)
	default:
		return clipValue(fmt.Sprint(f.Value.Interface()))
	}
}

func formatSecret(f Field) string {
	if f.Type.Kind() == reflect.Slice {
		n, bad := 0, 0
		for i := 0; i < f.Value.Len(); i++ {
			s, ok := f.Value.Index(i).Interface().(Secret)
			if !ok {
				continue
			}
			n++
			if _, err := s.Resolve(); err != nil {
				bad++
			}
		}
		if bad > 0 {
			return fmt.Sprintf("%d 把（其中 %d 把解析失败）", n, bad)
		}
		return fmt.Sprintf("%d 把", n)
	}
	s, _ := f.Value.Interface().(Secret)
	raw := strings.TrimSpace(string(s))
	resolved, err := s.Resolve()
	if err != nil {
		return "（未设置）"
	}
	switch {
	case strings.HasPrefix(raw, "file:"):
		return fmt.Sprintf("file:%s（%d 字节）", clipValue(strings.TrimPrefix(raw, "file:")), len(resolved))
	case strings.HasPrefix(raw, "$"):
		return fmt.Sprintf("%s（%d 字节）", raw, len(resolved))
	default:
		return fmt.Sprintf("明文（%d 字节）", len(resolved))
	}
}

func clipValue(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= 80 {
		return s
	}
	return string([]rune(s)[:80]) + "…"
}
