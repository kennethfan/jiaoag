package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kennethfan/jiaoag/internal/patch"
)

// SetOptions 描述一次 config set。
type SetOptions struct {
	// Path 是配置文件路径，空表示默认路径。
	Path string
	// Key 是 Fields 里的点号路径，例如 providers[0].model。
	Key string
	// Value 是新的字面值（字符串形态，按字段类型解析）。
	Value string
	// Apply 为 false 时只生成 diff（dry-run）。
	Apply bool
}

// SetResult 是 config set 的结果。
type SetResult struct {
	File    string
	Diff    string
	Backup  string
	Changed bool
}

// Set 在 yaml.Node 上改一个已知字段：注释与顺序尽量保留，写前给 diff，
// 写后立刻校验，校验失败自动回滚。
//
// 只允许已存在的字段路径（Fields 的索引），未知路径直接报错并给出最相近的
// 候选：写进一个拼错的键比报错更糟，因为它看起来成功了但永远不生效。
func Set(opts SetOptions) (SetResult, error) {
	path := opts.Path
	if path == "" {
		path = DefaultPath()
	}
	p, err := ExpandHome(path)
	if err != nil {
		return SetResult{}, err
	}
	old, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return SetResult{}, fmt.Errorf("配置文件不存在：%s\n先运行 `jiaoag serve -init` 生成样例", p)
		}
		return SetResult{}, fmt.Errorf("读取配置 %s 失败: %w", p, err)
	}
	cfg, err := Load(p)
	if err != nil {
		return SetResult{}, err
	}
	field, ok, near := FindField(cfg, opts.Key)
	if !ok {
		msg := fmt.Sprintf("未知配置项 %q", opts.Key)
		if near != "" {
			msg += fmt.Sprintf("，最相近的是 %q", near)
		}
		return SetResult{}, fmt.Errorf("%s。用 `jiaoag config show` 查看全部路径", msg)
	}
	segs, err := parseKeyPath(opts.Key)
	if err != nil {
		return SetResult{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(old, &doc); err != nil {
		return SetResult{}, fmt.Errorf("解析配置 %s 失败: %w", p, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return SetResult{}, fmt.Errorf("配置文件 %s 为空，无法按路径修改", p)
	}
	newNode, err := scalarNode(opts.Value, field.Type)
	if err != nil {
		return SetResult{}, fmt.Errorf("%s: %w", opts.Key, err)
	}
	valueText, err := encodeScalar(newNode)
	if err != nil {
		return SetResult{}, err
	}
	if strings.Contains(valueText, "\n") {
		return SetResult{}, fmt.Errorf("值不能包含换行；多行内容请用 `jiaoag config edit` 修改")
	}
	updated, err := spliceValue(old, doc.Content[0], segs, valueText)
	if err != nil {
		return SetResult{}, err
	}
	res := SetResult{File: p}
	if bytes.Equal(old, updated) {
		res.Diff = "（与当前配置相同，没有改动）"
		return res, nil
	}
	res.Changed = true
	if diff, _, derr := patch.Unified(filepath.Base(p), old, updated, true); derr == nil {
		res.Diff = diff
	}
	if !opts.Apply {
		return res, nil
	}

	// 先写临时文件并校验：写坏配置的代价是下次启动失败，必须在替换前拦住。
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.yaml")
	if err != nil {
		return SetResult{}, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return SetResult{}, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return SetResult{}, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return SetResult{}, fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if verr := validateConfigFile(tmpName); verr != nil {
		// 原文件本来就有问题时不阻止修改：否则用户连修配置的工具都没有。
		if origErr := validateConfigFile(p); origErr == nil || origErr.Error() != verr.Error() {
			return SetResult{}, fmt.Errorf("改完的配置校验失败，未写入：%w", verr)
		}
	}

	backup := p + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(backup, old, 0o600); err != nil {
		return SetResult{}, fmt.Errorf("写备份失败: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return SetResult{}, fmt.Errorf("替换配置失败: %w", err)
	}
	res.Backup = backup
	return res, nil
}

// validateConfigFile 用与启动一致的规则校验一份配置：配了 providers 就是
// 服务端配置，否则按客户端校验（只有 token 与传输相关字段）。
func validateConfigFile(path string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	if len(cfg.Providers) > 0 {
		return cfg.ValidateServer()
	}
	return cfg.ValidateClient()
}

type pathSeg struct {
	key   string
	index int
	isIdx bool
}

func parseKeyPath(p string) ([]pathSeg, error) {
	parts := strings.Split(p, ".")
	segs := make([]pathSeg, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("配置项路径 %q 里有空段", p)
		}
		if strings.HasSuffix(part, "]") {
			open := strings.IndexByte(part, '[')
			if open <= 0 {
				return nil, fmt.Errorf("配置项路径 %q 的下标写法不对", p)
			}
			idx, err := strconv.Atoi(part[open+1 : len(part)-1])
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("配置项路径 %q 的下标不是非负整数", p)
			}
			segs = append(segs, pathSeg{key: part[:open]}, pathSeg{index: idx, isIdx: true})
			continue
		}
		segs = append(segs, pathSeg{key: part})
	}
	return segs, nil
}

// spliceValue 只替换文件里那一段值文本：不重新序列化整份 YAML，所以空行、
// 注释位置与缩进都原样保留。
func spliceValue(raw []byte, root *yaml.Node, segs []pathSeg, valueText string) ([]byte, error) {
	node, parent, missing, err := walkPath(root, segs)
	if err != nil {
		return nil, err
	}
	lines := splitKeepEnds(string(raw))
	if missing {
		return insertKey(lines, parent, segs[len(segs)-1].key, valueText)
	}
	if node.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("只能修改标量字段；这里是嵌套结构，请用 `jiaoag config edit`")
	}
	if node.Line <= 0 || node.Line > len(lines) {
		return nil, fmt.Errorf("无法定位第 %d 行，请用 `jiaoag config edit` 修改", node.Line)
	}
	oldText, err := encodeScalar(node)
	if err != nil {
		return nil, err
	}
	if strings.Contains(oldText, "\n") {
		return nil, fmt.Errorf("当前值是多行块，请用 `jiaoag config edit` 修改")
	}
	runes := []rune(lines[node.Line-1])
	start := node.Column - 1
	end := start + len([]rune(oldText))
	if start < 0 || end > len(runes) {
		return nil, fmt.Errorf("无法定位值的边界，请用 `jiaoag config edit` 修改")
	}
	lines[node.Line-1] = string(runes[:start]) + valueText + string(runes[end:])
	return []byte(strings.Join(lines, "")), nil
}

func walkPath(root *yaml.Node, segs []pathSeg) (*yaml.Node, *yaml.Node, bool, error) {
	n := root
	for n != nil && n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil, nil, false, fmt.Errorf("文档为空")
		}
		n = n.Content[0]
	}
	for i, seg := range segs {
		if n == nil {
			return nil, nil, false, fmt.Errorf("路径在 %q 处断开", seg.key)
		}
		if seg.isIdx {
			if n.Kind != yaml.SequenceNode {
				return nil, nil, false, fmt.Errorf("%q 不是列表", seg.key)
			}
			if seg.index >= len(n.Content) {
				return nil, nil, false, fmt.Errorf("%q 只有 %d 项，下标 %d 越界；新增条目请用 `jiaoag config edit`",
					seg.key, len(n.Content), seg.index)
			}
			if i == len(segs)-1 {
				return n.Content[seg.index], n, false, nil
			}
			n = n.Content[seg.index]
			continue
		}
		if n.Kind != yaml.MappingNode {
			return nil, nil, false, fmt.Errorf("%q 不是映射", seg.key)
		}
		matched := false
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value != seg.key {
				continue
			}
			if i == len(segs)-1 {
				return n.Content[j+1], n, false, nil
			}
			n = n.Content[j+1]
			matched = true
			break
		}
		if matched {
			continue
		}
		if i == len(segs)-1 {
			return nil, n, true, nil
		}
		return nil, nil, false, fmt.Errorf("路径中间的 %q 不存在，无法继续写入", seg.key)
	}
	return n, nil, false, nil
}

// insertKey 在父映射的最后一行之后插入一行；缩进取自同级的第一个键。
func insertKey(lines []string, parent *yaml.Node, key, valueText string) ([]byte, error) {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("目标不是映射，无法插入 %q；请用 `jiaoag config edit`", key)
	}
	indent := parent.Column - 1 + 2
	if len(parent.Content) >= 2 {
		indent = parent.Content[0].Column - 1
	}
	if indent < 0 {
		indent = 0
	}
	last := endLine(parent)
	if last <= 0 {
		return nil, fmt.Errorf("无法定位父级行的位置，请用 `jiaoag config edit`")
	}
	if last > len(lines) {
		last = len(lines)
	}
	if last > 0 && !strings.HasSuffix(lines[last-1], "\n") {
		lines[last-1] += "\n"
	}
	newLine := strings.Repeat(" ", indent) + key + ": " + valueText + "\n"
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:last]...)
	out = append(out, newLine)
	out = append(out, lines[last:]...)
	return []byte(strings.Join(out, "")), nil
}

func endLine(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	max := n.Line
	for _, c := range n.Content {
		if e := endLine(c); e > max {
			max = e
		}
	}
	return max
}

// splitKeepEnds 保留每行结尾的换行，使拼接后除目标行外逐字节不变。
func splitKeepEnds(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func encodeScalar(n *yaml.Node) (string, error) {
	b, err := yaml.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("序列化值失败: %w", err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

func scalarNode(raw string, typ reflect.Type) (*yaml.Node, error) {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	raw = strings.TrimSpace(raw)
	if typ == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: raw}, nil
	}
	switch typ.Kind() {
	case reflect.Bool:
		switch strings.ToLower(raw) {
		case "true", "yes", "on", "1":
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}, nil
		case "false", "no", "off", "0":
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}, nil
		default:
			return nil, fmt.Errorf("需要布尔值（true/false），收到 %q", raw)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return nil, fmt.Errorf("需要整数，收到 %q", raw)
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: raw}, nil
	case reflect.Float32, reflect.Float64:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return nil, fmt.Errorf("需要数字，收到 %q", raw)
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: raw}, nil
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: raw}, nil
	}
}
