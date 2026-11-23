// Package project 发现工作区里的项目级扩展：提示片段、技能与 MCP server。
//
// 形态走业界惯例而不是自定义清单：根目录 AGENTS.md 是提示，.jiaoag/ 与
// .agents/ 下放 jiaoag 自己的东西。发现本身是纯读操作，信任判定留给调用方——
// 包不知道数据库，也不该知道。
//
// 安全前提（ADR-0025）：这些都是「仓库内容」，克隆一个仓库就等于把这些
// 文件交到 agent 面前。因此 Discover 只负责如实列出，是否生效由 app 按
// workspace + 内容摘要决定。
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DirName 是 jiaoag 自己的项目级目录。
	DirName = ".jiaoag"
	// AgentsFile 是根目录的项目提示文件（agent 生态惯例）。
	AgentsFile = "AGENTS.md"
	// PromptsDir 是 DirName 下的提示片段目录，按文件名排序拼接。
	PromptsDir = "prompts"
	// SkillsDir 是 DirName 下的技能目录，结构与 ~/.agents/skills 一致。
	SkillsDir = "skills"
	// SharedSkillsDir 是跨 agent 共用的技能目录（相对工作区根），也一并扫描。
	SharedSkillsDir = ".agents/skills"
	// MCPFile 是 DirName 下的 MCP server 清单。
	MCPFile = "mcp.yaml"

	// maxPromptFileBytes 是单个提示片段的上限。
	maxPromptFileBytes = 16 << 10
	// maxPromptBytes 是全部提示片段的上限：仓库不该用提示把上下文预算吃掉。
	maxPromptBytes = 32 << 10
	// SkillFile 是技能目录里必须存在的入口文件。
	SkillFile = "SKILL.md"
)

// Prompt 是一段项目提示。Path 是相对工作区根的路径，用于展示与审计。
type Prompt struct {
	Path string
	Text string
}

// Server 是一台项目级 MCP server，字段含义与全局配置一致。
type Server struct {
	Name    string
	Argv    []string
	Env     []string
	Dir     string
	Timeout time.Duration
	Trust   bool
}

// Project 是一次发现的结果。零值（Empty 为真）表示这个工作区没有项目级扩展。
type Project struct {
	Root      string
	Prompts   []Prompt
	SkillDirs []string
	Servers   []Server
	// Digest 覆盖全部项目资产的内容与路径；没有资产时为空。
	Digest string
}

// Empty 报告是否什么都没发现。
func (p *Project) Empty() bool {
	return p == nil || (len(p.Prompts) == 0 && len(p.SkillDirs) == 0 && len(p.Servers) == 0)
}

// Summary 返回一行中文描述，用于会话创建时的提示。
func (p *Project) Summary() string {
	if p.Empty() {
		return ""
	}
	var parts []string
	if len(p.Prompts) > 0 {
		parts = append(parts, fmt.Sprintf("提示 %d 个", len(p.Prompts)))
	}
	if len(p.SkillDirs) > 0 {
		parts = append(parts, fmt.Sprintf("技能 %d 个", len(p.SkillDirs)))
	}
	if len(p.Servers) > 0 {
		parts = append(parts, fmt.Sprintf("MCP %d 台", len(p.Servers)))
	}
	return strings.Join(parts, "、")
}

// PromptPaths 返回提示片段的相对路径。
func (p *Project) PromptPaths() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Prompts))
	for _, prompt := range p.Prompts {
		out = append(out, prompt.Path)
	}
	return out
}

// SkillNames 返回项目技能的目录名（不解析 frontmatter，展示够用）。
func (p *Project) SkillNames() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.SkillDirs))
	for _, dir := range p.SkillDirs {
		out = append(out, filepath.Base(dir))
	}
	return out
}

// ServerNames 返回 "name: argv" 形式，信任确认时要让人看清会执行什么。
func (p *Project) ServerNames() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Servers))
	for _, srv := range p.Servers {
		out = append(out, srv.Name+": "+strings.Join(srv.Argv, " "))
	}
	return out
}

// Discover 扫描工作区里的项目级扩展。
//
// 读不了的文件只报错不中断：项目扩展是增强，不是运行前提；调用方把错误
// 记进日志即可。返回的 Project 永远非 nil。
func Discover(root string) (*Project, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("project: 解析工作区路径: %w", err)
	}
	p := &Project{Root: abs}
	var errs []error

	if prompt, ok, err := readAgents(abs); err != nil {
		errs = append(errs, err)
	} else if ok {
		p.Prompts = append(p.Prompts, prompt)
	}
	prompts, perrs := readPrompts(abs, promptBytes(p.Prompts))
	p.Prompts = append(p.Prompts, prompts...)
	errs = append(errs, perrs...)

	p.SkillDirs = skillDirs(abs)
	servers, serr := readServers(abs)
	if serr != nil {
		errs = append(errs, serr)
	}
	p.Servers = servers

	if !p.Empty() {
		p.Digest = digest(abs, p)
	}
	return p, errors.Join(errs...)
}

func readAgents(root string) (Prompt, bool, error) {
	path := filepath.Join(root, AgentsFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Prompt{}, false, nil
		}
		return Prompt{}, false, fmt.Errorf("project: 读取 %s: %w", AgentsFile, err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return Prompt{}, false, nil
	}
	return Prompt{Path: AgentsFile, Text: clipBytes(text, maxPromptFileBytes, AgentsFile)}, true, nil
}

// readPrompts 读取 .jiaoag/prompts/*.md，按文件名排序（前缀数字即顺序）。
//
// used 是已经被 AGENTS.md 占掉的字节数：预算按全部提示算，谁靠前谁先得，
// 后面的截断而不是丢掉——宁可少一段也不能让项目提示静默消失。
func readPrompts(root string, used int) ([]Prompt, []error) {
	dir := filepath.Join(root, DirName, PromptsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("project: 读取 %s: %w", dir, err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var (
		out  []Prompt
		errs []error
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		room := maxPromptBytes - used
		if room <= 0 {
			break
		}
		rel := filepath.ToSlash(filepath.Join(DirName, PromptsDir, entry.Name()))
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("project: 读取 %s: %w", rel, err))
			continue
		}
		text := strings.TrimSpace(string(raw))
		if text == "" {
			continue
		}
		text = clipBytes(text, min(room, maxPromptFileBytes), rel)
		used += len(text)
		out = append(out, Prompt{Path: rel, Text: text})
	}
	return out, errs
}

func promptBytes(prompts []Prompt) int {
	total := 0
	for _, prompt := range prompts {
		total += len(prompt.Text)
	}
	return total
}

// skillDirs 返回存在的项目技能目录（绝对路径），按固定顺序。
func skillDirs(root string) []string {
	var out []string
	for _, rel := range []string{filepath.Join(DirName, SkillsDir), SharedSkillsDir} {
		dir := filepath.Join(root, rel)
		if containsSkill(dir) {
			out = append(out, dir)
		}
	}
	return out
}

func containsSkill(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name(), SkillFile)); err == nil {
			return true
		}
	}
	return false
}

// mcpDoc 是 .jiaoag/mcp.yaml 的形状；与全局配置的 mcp.servers 同构。
type mcpDoc struct {
	Servers []struct {
		Name    string            `yaml:"name"`
		Command string            `yaml:"command"`
		Args    []string          `yaml:"args"`
		Env     map[string]string `yaml:"env"`
		Dir     string            `yaml:"dir"`
		Timeout int               `yaml:"timeout_s"`
		Trust   string            `yaml:"trust"`
		Enabled *bool             `yaml:"enabled"`
	} `yaml:"servers"`
}

func readServers(root string) ([]Server, error) {
	path := filepath.Join(root, DirName, MCPFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("project: 读取 %s: %w", DirName+"/"+MCPFile, err)
	}
	var doc mcpDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("project: 解析 %s/%s: %w", DirName, MCPFile, err)
	}
	label := DirName + "/" + MCPFile
	seen := map[string]bool{}
	out := make([]Server, 0, len(doc.Servers))
	for i, srv := range doc.Servers {
		if srv.Enabled != nil && !*srv.Enabled {
			continue
		}
		name := strings.TrimSpace(srv.Name)
		if !validServerName(name) {
			return nil, fmt.Errorf("project: %s/%s 第 %d 台 server 的 name %q 非法（只允许字母数字-_，最多 32 字符）",
				DirName, MCPFile, i+1, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("project: %s/%s 里 server 名称 %q 重复", DirName, MCPFile, name)
		}
		seen[name] = true
		command := strings.TrimSpace(srv.Command)
		if command == "" {
			return nil, fmt.Errorf("project: %s 里 server %q 缺少 command", label, name)
		}
		timeout := srv.Timeout
		if timeout < 0 || timeout > 600 {
			return nil, fmt.Errorf("project: %s 里 server %q 的 timeout_s 必须在 0–600 之间", label, name)
		}
		switch srv.Trust {
		case "", "ask", "allow":
		default:
			return nil, fmt.Errorf("project: %s 里 server %q 的 trust 只支持 ask 或 allow", label, name)
		}
		dir := strings.TrimSpace(srv.Dir)
		if dir != "" {
			if strings.HasPrefix(dir, "~") {
				home, herr := os.UserHomeDir()
				if herr != nil {
					return nil, fmt.Errorf("project: %s 里 server %q 的 dir: %w", label, name, herr)
				}
				dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(root, dir)
			}
		}
		out = append(out, Server{
			Name:    name,
			Argv:    append([]string{command}, srv.Args...),
			Env:     sortedEnv(srv.Env),
			Dir:     dir,
			Timeout: time.Duration(timeout) * time.Second,
			// 项目级默认 ask：仓库内容不该默认为可信，ADR-0025。
			Trust: srv.Trust == "allow",
		})
	}
	return out, nil
}

func sortedEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func validServerName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// digest 对「路径 + 内容」排序后取哈希。
//
// 路径必须进摘要：把提示从 10-a.md 改名成 20-a.md 会改变装配顺序，那同样是
// 一次生效中的变化，必须让用户重新确认。
func digest(root string, p *Project) string {
	var lines []string
	for _, prompt := range p.Prompts {
		lines = append(lines, "prompt\t"+prompt.Path+"\t"+hashText(prompt.Text))
	}
	for _, dir := range p.SkillDirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			rel = dir
		}
		rel = filepath.ToSlash(rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			doc := filepath.Join(dir, entry.Name(), SkillFile)
			raw, err := os.ReadFile(doc)
			if err != nil {
				continue
			}
			lines = append(lines, "skill\t"+rel+"/"+entry.Name()+"\t"+hashText(string(raw)))
		}
	}
	for _, srv := range p.Servers {
		lines = append(lines, "mcp\t"+srv.Name+"\t"+hashText(strings.Join(srv.Argv, "\x00")+"\x00"+strings.Join(srv.Env, "\x00")+"\x00"+srv.Dir))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// clipBytes 按字节上限截断并标注，尽量不切在 UTF-8 字符中间。
func clipBytes(text string, limit int, label string) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "\n\n（" + label + " 超过 " + strconv.Itoa(limit) + " 字节，已截断。请到该文件查看完整内容）"
}
