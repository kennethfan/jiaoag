package perm

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kennethfan/jiaoag/internal/policyfeed"
	"github.com/kennethfan/jiaoag/internal/tools"
)

//go:embed default.yaml
var defaultPolicy []byte

// 防止规则文件被写坏后拖慢每次工具调用（ADR-0010 的工程约束）。
const (
	maxRulesPerFile = 2000
	maxRuleBytes    = 256
)

// Rule 是一条声明式允许/收紧规则。规则是数据，判定顺序在代码里（ADR-0010）。
//
// Programs 匹配 argv[0] 的 basename；Subcommands 非空时要求 argv[1] 命中。
// Tighten 为 true 的规则只能让命令落到审批，不能放行。
type Rule struct {
	ID          string   `yaml:"id" json:"id"`
	Tool        string   `yaml:"tool,omitempty" json:"tool,omitempty"`
	Programs    []string `yaml:"programs,omitempty" json:"programs,omitempty"`
	Subcommands []string `yaml:"subcommands,omitempty" json:"subcommands,omitempty"`
	// AllowFlags 非空时，argv 里的 flag 必须命中这里（`--` 始终允许）。
	// 合并短 flag（如 -la）按字母逐个匹配，不必枚举每种组合。
	AllowFlags []string `yaml:"allow_flags,omitempty" json:"allow_flags,omitempty"`
	// DenyFlags 显式拒绝的 flag（--output、-exec 等），优先于 AllowFlags。
	DenyFlags []string `yaml:"deny_flags,omitempty" json:"deny_flags,omitempty"`
	// PathArgs 为 true 时，所有非 flag 参数必须落在命令 cwd 之内。
	PathArgs bool   `yaml:"path_args,omitempty" json:"path_args,omitempty"`
	Reason   string `yaml:"reason,omitempty" json:"reason,omitempty"`
	// Tighten 由 Loader 强制置位：外部文件只能收紧，不能放宽。
	Tighten bool `yaml:"-" json:"tighten,omitempty"`
}

type ruleFile struct {
	Version string `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

// Ruleset 是不可变快照：Loader 产出新快照后原子替换指针。
type Ruleset struct {
	Version string
	Hash    string
	Rules   []Rule
}

// NewRuleset 规范化条目顺序并计算指纹。排序让 Hash 与文件里的书写顺序无关。
func NewRuleset(version string, rules []Rule) *Ruleset {
	cp := append([]Rule(nil), rules...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	return &Ruleset{Version: version, Hash: hashRules(version, cp), Rules: cp}
}

// DefaultRuleset 返回内嵌规则集。解析失败时返回空规则集——失败关闭，
// 不会因为一个坏文件而放行任何命令。
func DefaultRuleset() *Ruleset {
	var f ruleFile
	if err := yaml.Unmarshal(defaultPolicy, &f); err != nil {
		return NewRuleset("builtin-invalid", nil)
	}
	return NewRuleset(f.Version, f.Rules)
}

// Loader 读内嵌默认规则，再叠加 policy.d/*.yaml 的收紧规则。
type Loader struct {
	dir string
	// feedKeys 非空时，目录里的 feed.yaml 必须是这些公钥签过的，且未过期。
	feedKeys policyfeed.Keys
}

// NewLoader 返回规则加载器；dir 为空表示只用内嵌规则。
func NewLoader(dir string) *Loader { return &Loader{dir: dir} }

// WithFeedKeys 启用签名规则 feed：读取 policy.d/feed.yaml 前先验签。
//
// 验签失败或 feed 过期都直接报错，不静默跳过：静默跳过等于「收紧规则悄悄没了」，
// 用户会以为自己受保护。
func (l *Loader) WithFeedKeys(keys policyfeed.Keys) *Loader {
	if l == nil {
		return l
	}
	l.feedKeys = keys
	return l
}

// Load 读取并校验规则。外部文件解析失败直接报错：静默跳过会让用户以为
// 自己的收紧规则生效了，而实际上没有。
func (l *Loader) Load() (*Ruleset, error) {
	var base ruleFile
	if err := yaml.Unmarshal(defaultPolicy, &base); err != nil {
		return nil, fmt.Errorf("perm: 解析内嵌规则: %w", err)
	}
	if err := validateRules(base.Rules); err != nil {
		return nil, fmt.Errorf("perm: 内嵌规则: %w", err)
	}

	rules := append([]Rule(nil), base.Rules...)
	version := base.Version

	// feed 先验签：它和本地规则同目录，但不是同一类东西——本地文件是用户自己
	// 写的，feed 是外部发布的，必须证明来源。
	var feedData []byte
	var feedMeta policyfeed.Metadata
	if l != nil && len(l.feedKeys) > 0 {
		if l.dir == "" {
			return nil, errors.New("perm: 配置了规则 feed 公钥，但没有规则目录")
		}
		data, meta, err := policyfeed.LoadFile(l.dir, l.feedKeys)
		if err != nil {
			return nil, err
		}
		if !meta.ExpiresAt.IsZero() && time.Now().After(meta.ExpiresAt) {
			return nil, fmt.Errorf("perm: 规则 feed 已于 %s 过期，请运行 jiaoag policy sync 更新",
				meta.ExpiresAt.Format(time.RFC3339))
		}
		feedData, feedMeta = data, meta
	}

	if l != nil && l.dir != "" {
		names, err := policyFiles(l.dir)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			// feed 已经单独验签加载，不能再按普通本地文件读一遍。
			if feedData != nil && name == policyfeed.FileName {
				continue
			}
			body, err := os.ReadFile(filepath.Join(l.dir, name))
			if err != nil {
				return nil, fmt.Errorf("perm: 读取规则 %s: %w", name, err)
			}
			var f ruleFile
			if err := yaml.Unmarshal(body, &f); err != nil {
				return nil, fmt.Errorf("perm: 解析规则 %s: %w", name, err)
			}
			if err := validateRules(f.Rules); err != nil {
				return nil, fmt.Errorf("perm: 规则 %s: %w", name, err)
			}
			for _, rule := range f.Rules {
				// 外部规则只能收紧：这条用类型/加载逻辑表达，不靠文档约定。
				rule.Tighten = true
				rules = append(rules, rule)
			}
			if f.Version != "" {
				version += "+" + f.Version
			}
		}
	}

	// feed 排在本机规则之后：本机规则是用户明确写下的，先匹配；feed 是补充。
	if feedData != nil {
		var f ruleFile
		if err := yaml.Unmarshal(feedData, &f); err != nil {
			return nil, fmt.Errorf("perm: 解析规则 feed: %w", err)
		}
		if err := validateRules(f.Rules); err != nil {
			return nil, fmt.Errorf("perm: 规则 feed: %w", err)
		}
		for _, rule := range f.Rules {
			rule.Tighten = true
			rules = append(rules, rule)
		}
		version += "+feed@" + feedMeta.Version
	}
	return NewRuleset(version, rules), nil
}

// policyFiles 返回目录下按名字排序的 yaml 文件；目录不存在不是错误。
func policyFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("perm: 读取规则目录 %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func validateRules(rules []Rule) error {
	if len(rules) > maxRulesPerFile {
		return fmt.Errorf("规则条数 %d 超过上限 %d", len(rules), maxRulesPerFile)
	}
	seen := make(map[string]bool, len(rules))
	for i, rule := range rules {
		if strings.TrimSpace(rule.ID) == "" {
			return fmt.Errorf("rules[%d] 缺少 id", i)
		}
		if len(rule.ID) > maxRuleBytes {
			return fmt.Errorf("rules[%d] 的 id 超过 %d 字节", i, maxRuleBytes)
		}
		if seen[rule.ID] {
			return fmt.Errorf("规则 id 重复：%s", rule.ID)
		}
		seen[rule.ID] = true
		if len(rule.Programs) == 0 {
			return fmt.Errorf("规则 %s 没有 programs（v1 只支持程序白名单）", rule.ID)
		}
	}
	return nil
}

// Hash 覆盖版本与全部规则内容：规则顺序不影响结果。
func hashRules(version string, rules []Rule) string {
	h := sha256.New()
	fmt.Fprintf(h, "version=%s\n", version)
	for _, rule := range rules {
		fmt.Fprintf(h, "id=%s|tool=%s|programs=%s|subcommands=%s|allow_flags=%s|deny_flags=%s|path_args=%t|tighten=%t|reason=%s\n",
			rule.ID, rule.Tool, strings.Join(rule.Programs, ","),
			strings.Join(rule.Subcommands, ","), strings.Join(rule.AllowFlags, ","),
			strings.Join(rule.DenyFlags, ","), rule.PathArgs, rule.Tighten, rule.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Env 是判定时可见的环境事实。插件式规则只能读它，不能自己探测。
type Env struct {
	OS    string
	Shell string
}

// TightenVerdict 是外部规则唯一能给出的结论集合。
//
// 它刻意不是 Verdict：外部规则没有「放行」这个取值可用，放宽只能改本地规则
// （ADR-0010）。这条约束由类型表达，而不是靠文档约定。
type TightenVerdict string

// TightenRequireUser 表示外部规则要求这次调用落到审批。
const TightenRequireUser TightenVerdict = "require_user"

// Tighten 是外部规则唯一能给出的结论：只能要求审批，不能放行。
type Tighten struct {
	Verdict TightenVerdict
	Reason  string
	RuleID  string
	Source  string
}

// TightenRule 是 v2 的第三方规则接口（ADR-0010）。v1 只保留形状，不加载实现。
type TightenRule interface {
	Evaluate(ctx context.Context, p tools.Prepared, env Env) *Tighten
}

// Evaluate 是命令与工具的唯一判定入口。
//
// 判定顺序（短路）：
//  1. 写入提案 → 必审批（M4 的 diff 审批）；
//  2. 只读 → 放行（read_output 与三个 git 只读工具走这里）；
//  3. 命令 → 元字符 + 白名单；不做任何「清洗后匹配」；
//  4. 外部工具 → 只有本地配置的 trust 能放行，其余必审批；
//  5. 其余 → 必审批。
func (r *Ruleset) Evaluate(ctx context.Context, p tools.Prepared) Decision {
	if err := ctx.Err(); err != nil {
		return Decision{Verdict: RequireUser, Reason: "context_canceled: " + err.Error()}
	}
	if p.Change != nil {
		return Decision{Verdict: RequireUser, Reason: "tool_writes: 写入必须经用户批准", PolicyVersion: r.version()}
	}
	// 收紧规则先于只读短路：本地策略必须能让 git 只读工具也落到审批。
	if d := r.tighten(p.Call.Name, p.Command); d != nil {
		return *d
	}
	// 外部工具不能靠 spec.Effects 自证安全：声明 read 不等于真的只读。
	// 唯一能放行的是本地配置的 trust=allow，且必须留审计。
	if p.External != nil {
		if p.External.Trusted {
			return Decision{
				Verdict:       Allow,
				Reason:        "external_trusted: 本地配置放行了该外部工具",
				PolicyVersion: r.version(),
				RuleID:        "external_trusted",
			}
		}
		return Decision{
			Verdict:       RequireUser,
			Reason:        "external_tool: 外部工具调用必须经用户批准",
			PolicyVersion: r.version(),
		}
	}
	writes, onlyRead := effectsClass(p.Spec)
	if writes {
		return Decision{Verdict: RequireUser, Reason: "tool_writes: 写入必须经用户批准", PolicyVersion: r.version()}
	}
	if onlyRead {
		// 只读放行同样要带规则版本：git 工具每次执行都会写 exec.allow 审计。
		return Decision{
			Verdict:       Allow,
			Reason:        "read_only: 只读工具自动放行",
			PolicyVersion: r.version(),
			RuleID:        "read_only",
		}
	}
	if p.Command != nil {
		return r.evaluateCommand(p.Call.Name, p.Command)
	}
	return Decision{Verdict: RequireUser, Reason: "unknown_effect: 未识别的副作用按最危险的算"}
}

// evaluateCommand 依次检查元字符、收紧规则、白名单。默认拒绝。
func (r *Ruleset) evaluateCommand(toolName string, cmd *tools.Command) Decision {
	argv := cmd.Argv
	if len(argv) == 0 {
		return Decision{Verdict: RequireUser, Reason: "empty_argv: 命令为空"}
	}
	// shell 命令要在元字符检查之前拦下：一条 shell 命令里的 | > $ 是正常
	// 语法，不是注入，拿元字符去解释只会给出误导性的理由。它同样无法按
	// 程序名匹配白名单（argv[0] 永远是 sh/cmd），所以固定要人看。
	if cmd.Shell != "" {
		return Decision{
			Verdict:       RequireUser,
			Reason:        "shell: 需要 shell 解释（管道/重定向/多命令），无法静态判定",
			PolicyVersion: r.version(),
		}
	}
	if token, ok := metacharToken(argv); ok {
		return Decision{Verdict: RequireUser, Reason: fmt.Sprintf("metachar: argv 含 shell 元字符 %q", token)}
	}
	base := filepath.Base(argv[0])
	if r == nil {
		return Decision{Verdict: RequireUser, Reason: "no_rules: 没有可用规则集"}
	}
	// 白名单只认程序名本身。`./go test`、`/tmp/x/go test` 的 basename 同样是
	// go，但执行的是工作区/任意路径上的程序——那不是白名单想放行的东西。
	if argv[0] != base {
		return Decision{
			Verdict:       RequireUser,
			Reason:        fmt.Sprintf("program_path: %q 含路径，白名单只接受程序名", argv[0]),
			PolicyVersion: r.Version,
		}
	}

	for _, rule := range r.Rules {
		if rule.Tighten || !ruleMatches(rule, toolName, base, argv) {
			continue
		}
		if reason, ok := ruleArgsVerdict(rule, argv, cmd.Cwd); !ok {
			return Decision{
				Verdict:       RequireUser,
				Reason:        "unsafe_args: " + reason,
				PolicyVersion: r.Version,
				RuleID:        rule.ID,
			}
		}
		return Decision{
			Verdict:       Allow,
			Reason:        "whitelist: " + rule.ID,
			PolicyVersion: r.Version,
			RuleID:        rule.ID,
		}
	}
	return Decision{
		Verdict:       RequireUser,
		Reason:        fmt.Sprintf("not_whitelisted: %s 不在白名单内", base),
		PolicyVersion: r.Version,
	}
}

// tighten 返回命中的本地收紧规则。它先于只读短路执行：外部策略必须能
// 让 git 只读工具也落到审批，否则 `TightenRule` 只是半个接口。
func (r *Ruleset) tighten(toolName string, cmd *tools.Command) *Decision {
	if r == nil || cmd == nil || len(cmd.Argv) == 0 {
		return nil
	}
	base := filepath.Base(cmd.Argv[0])
	for _, rule := range r.Rules {
		if !rule.Tighten || !ruleMatches(rule, toolName, base, cmd.Argv) {
			continue
		}
		reason := rule.Reason
		if reason == "" {
			reason = "本地策略要求审批"
		}
		return &Decision{
			Verdict:       RequireUser,
			Reason:        "tighten: " + reason,
			PolicyVersion: r.Version,
			RuleID:        rule.ID,
		}
	}
	return nil
}

func (r *Ruleset) version() string {
	if r == nil {
		return ""
	}
	return r.Version
}

// ruleArgsVerdict 是白名单的程序/子命令匹配之后的第二道约束：flag 必须在
// 允许列表里，路径参数必须在工作区根内。缺了它，`cat /etc/passwd` 与
// `go test -exec=...` 都会以「命中白名单」的名义自动放行。
func ruleArgsVerdict(rule Rule, argv []string, cwd string) (string, bool) {
	start := 1
	if len(rule.Subcommands) > 0 {
		start = 2
	}
	for _, token := range argv[start:] {
		if strings.HasPrefix(token, "-") {
			if token == "--" {
				continue
			}
			if flagDenied(rule.DenyFlags, token) {
				return fmt.Sprintf("flag %q 被规则拒绝", token), false
			}
			if len(rule.AllowFlags) > 0 && !flagAllowed(rule.AllowFlags, token) {
				return fmt.Sprintf("flag %q 不在允许列表", token), false
			}
			continue
		}
		if rule.PathArgs {
			if err := pathInside(cwd, token); err != nil {
				return err.Error(), false
			}
		}
	}
	return "", true
}

func flagDenied(denied []string, token string) bool {
	name := flagName(token)
	for _, d := range denied {
		if d == token || d == name {
			return true
		}
	}
	return false
}

func flagAllowed(allow []string, token string) bool {
	name := flagName(token)
	for _, a := range allow {
		if a == token || a == name {
			return true
		}
	}
	// 合并短 flag（-la）：拆成单字母后逐个检查。
	if strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--") && len(name) > 2 {
		for _, ch := range name[1:] {
			if !slices.Contains(allow, "-"+string(ch)) {
				return false
			}
		}
		return true
	}
	return false
}

func flagName(token string) string {
	if i := strings.IndexByte(token, '='); i >= 0 {
		return token[:i]
	}
	return token
}

// pathInside 拒绝逃出 cwd 的路径参数。词法检查之外，存在的路径还会做一次
// 软链解析后的检查：工作区里的链接不能成为阅读区外的跳板。
func pathInside(cwd, token string) error {
	if cwd == "" {
		return fmt.Errorf("无法校验路径 %q：缺少工作区根", token)
	}
	abs := token
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 不在工作区内", token)
	}
	realRoot, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil
	}
	rel, err = filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 指向工作区外", token)
	}
	return nil
}

func ruleMatches(rule Rule, toolName, base string, argv []string) bool {
	if rule.Tool != "" && rule.Tool != toolName {
		return false
	}
	ok := false
	for _, program := range rule.Programs {
		if program == base {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	if len(rule.Subcommands) == 0 {
		return true
	}
	if len(argv) < 2 {
		return false
	}
	for _, sub := range rule.Subcommands {
		if sub == argv[1] {
			return true
		}
	}
	return false
}

// metachars 是 ADR-0004 列出的 shell 元字符。命中只表示「值得让人看一眼」，
// 不是拒绝，更不能被当成「清洗后放行」的依据。
const metachars = "|&;<>$`()*?[]~"

func metacharToken(argv []string) (string, bool) {
	for _, token := range argv {
		if strings.ContainsAny(token, metachars) {
			return token, true
		}
		if strings.ContainsAny(token, "\n\r") {
			return token, true
		}
	}
	return "", false
}
