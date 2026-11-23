// Package skills 加载「按需阅读」的技能包。
//
// 一个技能就是一个目录，里面至少有 SKILL.md：YAML frontmatter 提供 name /
// description，正文是操作步骤。目录里通常还有脚本、参考文档。
//
// 设计取舍：**正文不进系统提示**。系统提示只放一份索引（名字 + 一句话），
// 模型判断用得上时再调用 skill 工具加载正文。技能库有几十个、几百兆时，
// 全量注入会直接吃掉预算；按需加载让它变成一次普通的读文件。
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// FileName 是技能目录里必须存在的入口文件。
	FileName = "SKILL.md"
	// maxIndexRunes 限制索引里单条描述的长度：索引要小，细节留给正文。
	// 65 个技能按 120 字算约 8 KB（≈2.7k token），再长就该裁剪技能库了。
	maxIndexRunes = 120
	// maxIndexBytes 是整段索引的硬上限：技能库失控时也要保住上下文预算。
	maxIndexBytes = 10 << 10
	// maxBodyBytes 是单次加载正文的硬上限，防止一个巨型 SKILL.md 把上下文打爆。
	maxBodyBytes = 96 << 10
	// maxEntries 是正文头部列出的目录项数量。
	maxEntries = 40
	// maxScanBytes 是解析 frontmatter 时最多读的字节数。
	maxScanBytes = 16 << 10
)

// Skill 是一个已发现的技能。
type Skill struct {
	Name        string
	Description string
	// Manual 为真表示 SKILL.md 声明了 disable-model-invocation: true：
	// 这类技能不让模型自己挑（作者本意是「只能由用户显式调起」），所以
	// 不进系统提示的索引，只留给 /skill 面板手动选。
	Manual bool
	// Dir 是技能目录的绝对路径；正文里的相对引用以它为准。
	Dir string
	// Source 是配置里写的目录（原样），用于回答「这个技能从哪来的」。
	Source string
}

// Catalog 是一次加载得到的技能集合，按名字索引。
type Catalog struct {
	skills []Skill
	index  map[string]Skill
}

// Load 扫描 dirs 下的每个子目录，找出带 SKILL.md 的技能。
//
// root 用来解析相对目录（相对工作区根）。同名的技能以先出现的为准——
// 顺序由调用方决定，通常是「机器级在前、项目级在后」，这样机器级不会被
// 某个仓库里的同名文件顶掉。读不了或格式坏掉的目录只记进 errors，不中断
// 加载：技能是增强，不是运行前提。
func Load(dirs []string, root string) (*Catalog, []error) {
	cat := &Catalog{index: make(map[string]Skill)}
	var errs []error
	seen := make(map[string]bool)
	for _, raw := range dirs {
		dir, err := resolveDir(raw, root)
		if err != nil {
			errs = append(errs, fmt.Errorf("技能目录 %q: %w", raw, err))
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("读取技能目录 %q: %w", raw, err))
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillDir := filepath.Join(dir, entry.Name())
			doc := filepath.Join(skillDir, FileName)
			meta, err := readMeta(doc)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("%s: %w", doc, err))
				}
				continue
			}
			name := strings.TrimSpace(meta.Name)
			if !validName(name) {
				name = entry.Name()
			}
			if _, dup := cat.index[name]; dup {
				continue
			}
			skill := Skill{
				Name:        name,
				Description: clip(strings.Join(strings.Fields(meta.Description), " "), maxIndexRunes),
				Manual:      meta.Manual,
				Dir:         skillDir,
				Source:      raw,
			}
			cat.skills = append(cat.skills, skill)
			cat.index[name] = skill
		}
	}
	sort.Slice(cat.skills, func(i, j int) bool { return cat.skills[i].Name < cat.skills[j].Name })
	return cat, errs
}

// Len 返回技能数量。
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.skills)
}

// Get 按名字查技能。
func (c *Catalog) Get(name string) (Skill, bool) {
	if c == nil {
		return Skill{}, false
	}
	s, ok := c.index[name]
	return s, ok
}

// Names 返回全部技能名（已排序）。
func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.skills))
	for _, s := range c.skills {
		out = append(out, s.Name)
	}
	return out
}

// Skills 返回全部技能（已按名字排序）的副本，供列表接口使用。
func (c *Catalog) Skills() []Skill {
	if c == nil {
		return nil
	}
	return append([]Skill(nil), c.skills...)
}

// Section 生成系统提示里的技能索引段；没有技能时返回空串。
func (c *Catalog) Section() string {
	if c.Len() == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 可用技能（按需加载）\n\n")
	b.WriteString("下面这些技能只给了名字和适用场景，正文不在上下文里。")
	b.WriteString("当任务与某个技能的适用场景相符时，先调用 `skill` 工具加载它的正文，再按正文操作；不相符就不要加载。\n\n")
	listed, manual := 0, 0
	for _, s := range c.skills {
		if s.Manual {
			manual++
		}
	}
	for _, s := range c.skills {
		if s.Manual {
			continue
		}
		if b.Len() > maxIndexBytes {
			b.WriteString(fmt.Sprintf("- …（还有 %d 个技能未列出，请缩小 skills_dirs 或精简描述）\n", len(c.skills)-listed-manual))
			break
		}
		listed++
		if s.Description == "" {
			b.WriteString("- `" + s.Name + "`\n")
			continue
		}
		b.WriteString("- `" + s.Name + "`：" + s.Description + "\n")
	}
	if manual > 0 {
		b.WriteString(fmt.Sprintf("\n另有 %d 个技能标记为「仅手动调用」（disable-model-invocation），没有列在上面；用户显式指定时用 `skill` 工具读取即可。\n", manual))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Body 读取技能正文，附带目录信息。
//
// 头部写明绝对目录与目录内容：技能正文里常见 `scripts/x.py` 这类相对引用，
// 不给基准目录模型只能猜。目录项限量，避免一个打包了 node_modules 的目录
// 把正文挤掉。
func (c *Catalog) Body(name string) (string, error) {
	s, ok := c.Get(name)
	if !ok {
		return "", fmt.Errorf("没有名为 %q 的技能", name)
	}
	doc := filepath.Join(s.Dir, FileName)
	raw, err := os.ReadFile(doc)
	if err != nil {
		return "", fmt.Errorf("读取 %s: %w", doc, err)
	}
	body, err := stripFrontmatter(string(raw))
	if err != nil {
		return "", fmt.Errorf("%s: %w", doc, err)
	}
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes] + "\n\n（正文超过 " + strconv.Itoa(maxBodyBytes) + " 字节，已截断）"
	}

	var b strings.Builder
	b.WriteString("技能：")
	b.WriteString(s.Name)
	b.WriteString("\n基准目录：")
	b.WriteString(s.Dir)
	b.WriteString("\n技能里的相对路径（如 scripts/foo.py、docs/x.md）都相对这个目录。")
	if entries := dirEntries(s.Dir); entries != "" {
		b.WriteString("\n目录内容：")
		b.WriteString(entries)
	}
	b.WriteString("\n\n---\n\n")
	b.WriteString(strings.TrimSpace(body))
	return b.String(), nil
}

type meta struct {
	Name        string
	Description string
	// Manual 对应 disable-model-invocation: true。
	Manual bool
}

// readMeta 只解析 frontmatter，不读整个文件。
func readMeta(path string) (meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return meta{}, err
	}
	defer f.Close()
	buf := make([]byte, maxScanBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return meta{}, err
	}
	head := string(buf[:n])
	front, _, ok := splitFrontmatter(head)
	if !ok {
		// 没有 frontmatter：用目录名当技能名，没有描述。宁可索引不全，
		// 也不因为一个手写文件格式不对就整台不可用。
		return meta{}, nil
	}
	return parseMeta(front), nil
}

// parseMeta 逐行取 name / description / disable-model-invocation，不用 YAML 解析器。
//
// 技能文件是手写的，几乎每个真实的技能库都有几份 frontmatter 不是合法
// YAML——最典型的是 description 里带未加引号的 `Triggers: a, b`。按整份
// YAML 解析会因为一条描述丢掉整个技能；这里只认几个键，其余原样跳过。
func parseMeta(front string) meta {
	var m meta
	lines := strings.Split(front, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "name" && key != "description" && key != "disable-model-invocation" {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "|" || value == ">" || value == "|-" || value == ">-" {
			var parts []string
			for i+1 < len(lines) {
				next := lines[i+1]
				if strings.TrimSpace(next) == "" {
					i++
					continue
				}
				if !strings.HasPrefix(next, " ") && !strings.HasPrefix(next, "\t") {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
				i++
			}
			value = strings.Join(parts, " ")
		}
		value = strings.TrimSpace(strings.Trim(value, `"'`))
		switch key {
		case "name":
			m.Name = value
		case "description":
			m.Description = value
		case "disable-model-invocation":
			// 各家技能库写的是 YAML 布尔：true / yes / on / 1 都认。
			switch strings.ToLower(value) {
			case "true", "yes", "on", "1":
				m.Manual = true
			}
		}
	}
	return m
}

// validName 判断技能名是否适合当工具参数：字母数字开头，只含 . _ -。
// 不合格（例如 frontmatter 写坏了）就退回目录名，避免索引里出现乱码。
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '_' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// splitFrontmatter 把文本切成 frontmatter 与正文。frontmatter 必须以 `---`
// 起止；缺失时 ok 为 false。
func splitFrontmatter(text string) (front, body string, ok bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", text, false
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", text, false
	}
	front = rest[:end]
	body = rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	return front, body, true
}

func stripFrontmatter(text string) (string, error) {
	_, body, ok := splitFrontmatter(text)
	if !ok {
		return text, nil
	}
	return body, nil
}

// dirEntries 列出技能目录的一级内容，目录加后缀 "/"。
func dirEntries(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	more := 0
	if len(names) > maxEntries {
		more = len(names) - maxEntries
		names = names[:maxEntries]
	}
	out := strings.Join(names, ", ")
	if more > 0 {
		out += fmt.Sprintf(" …（另有 %d 项）", more)
	}
	return out
}

// resolveDir 把配置里的目录解析成绝对路径：`~` 展开，相对路径按工作区根。
func resolveDir(dir, root string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("目录为空")
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
	}
	if !filepath.IsAbs(dir) {
		if root == "" {
			return "", fmt.Errorf("相对目录 %q 需要工作区根", dir)
		}
		dir = filepath.Join(root, dir)
	}
	return filepath.Clean(dir), nil
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
