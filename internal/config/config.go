// Package config 负责加载配置并解析密钥来源。
//
// 配置默认位于 ~/.config/jiaoag/config.yaml；所有路径支持 ~ 展开，
// 所有密钥支持三种写法：明文、$ENV_NAME、file:/abs/path。
package config

import (
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kennethfan/jiaoag/internal/policyfeed"
)

// AppName 同时用于目录名与二进制名前缀。
const AppName = "jiaoag"

//go:embed example.yaml
var exampleYAML []byte

// Config 是服务端与客户端共享的配置结构。
type Config struct {
	Server    Server     `yaml:"server"`
	Storage   Storage    `yaml:"storage"`
	Agent     Agent      `yaml:"agent"`
	Policy    Policy     `yaml:"policy"`
	Exec      Exec       `yaml:"exec"`
	Providers []Provider `yaml:"providers"`
	MCP       MCP        `yaml:"mcp"`
	Project   Project    `yaml:"project"`
}

// Project 控制工作区里的项目级扩展（AGENTS.md、.jiaoag/、.agents/skills）。
//
// 这些内容来自仓库，可能不是自己写的，所以默认 ask：检出项目内容后先确认
// 一次（按 workspace + 内容摘要记录），确认前整体跳过。auto 用于自己的
// 仓库或 CI，off 用于批量巡检不可信仓库。
type Project struct {
	Mode string `yaml:"mode"`
}

// 项目级扩展的生效模式。
const (
	ProjectAsk  = "ask"
	ProjectAuto = "auto"
	ProjectOff  = "off"
)

// MCP 是外部工具来源的启动配置（ADR-0014）。只支持 stdio。
type MCP struct {
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer 是一台 stdio MCP server。
type MCPServer struct {
	// Name 是工具前缀与审计来源，只允许 [A-Za-z0-9_-]。
	Name string `yaml:"name"`
	// Command 与 Args 拼接成 argv，不经过 shell。
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Env 是追加到宿主环境之上的变量（明文）；密钥建议用 ${VAR} 由调用方
	// 启动 server 前注入，而不是写进配置文件。
	Env map[string]string `yaml:"env"`
	// Dir 是子进程工作目录；支持 ~ 展开。
	Dir string `yaml:"dir"`
	// TimeoutSeconds 是单次 tools/call 的上限，默认 60，范围 1–600。
	TimeoutSeconds int `yaml:"timeout_s"`
	// Trust 取 allow（默认：配置即信任，调用免审但记审计）或 ask（每次审批）。
	Trust string `yaml:"trust"`
	// Tools 过滤这台 server 暴露的工具，名字用 server 自己的原名（不含 mcp__ 前缀）。
	Tools MCPServerTools `yaml:"tools"`
	// Enabled 默认 true；显式 false 用于临时停用而不删配置。
	Enabled *bool `yaml:"enabled"`
}

// MCPServerTools 决定哪些工具能进注册表。Include 非空时只保留列出的，
// 再减去 Exclude；两个列表都为空表示全部保留。过滤发生在连接之后、进注册表之前，
// 被剔掉的工具模型根本看不见（用来做只读接入，如 LSP server 的 edit_file）。
type MCPServerTools struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

// IsEnabled 返回是否启用，未配置视为启用。
func (s MCPServer) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// ToolFilter 返回去掉空白的工具过滤列表。名字按 server 自己的原名精确匹配
// （大小写敏感），不含 mcp__ 前缀。
func (s MCPServer) ToolFilter() (include, exclude []string) {
	return trimNames(s.Tools.Include), trimNames(s.Tools.Exclude)
}

func trimNames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, name := range in {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Policy 是本地权限规则的位置。外部规则只能收紧（ADR-0010）。
type Policy struct {
	Dir string `yaml:"dir"`
	// Feed 是可选的签名规则 feed（ADR-0032）。只收紧，所以来源不可信也不会
	// 放宽任何东西；但仍要求签名，避免有人往规则目录里塞文件。
	Feed PolicyFeed `yaml:"feed"`
}

// PolicyFeed 描述规则 feed 的来源与信任根。
type PolicyFeed struct {
	// URL 支持 http(s) 与 file://，签名约定在同址加 .sig 后缀。
	URL string `yaml:"url"`
	// Keys 是 ed25519 公钥（base64），可配多把用于轮换。
	Keys []string `yaml:"keys"`
}

// Exec 是命令执行的约束。
type Exec struct {
	// Backend 取 direct（v1 唯一实现）。降级必须显式，不能静默回退。
	Backend string `yaml:"backend"`
	// TimeoutSeconds 是单条命令的默认超时；单次调用可降到 1s、升到 600s。
	TimeoutSeconds int `yaml:"timeout_s"`
	// EventLimit 是进事件的输出上限，必须低于 agent 的 8 KB 截断阈值。
	EventLimit int `yaml:"event_limit"`
	// LogLimit 是全文日志的磁盘上限。
	LogLimit int `yaml:"log_limit"`
	// Limits 是单条命令的资源上限；零值表示不限制（保持历史行为）。
	Limits ExecLimits `yaml:"limits"`
}

// MinExecMemoryMB 是 memory_mb 的最小可配置值：再低会把普通的编译/测试
// 一起挡掉，比不设更烦人。
const MinExecMemoryMB = 64

// ExecLimits 是命令级资源限制。0 表示该项不限制。
type ExecLimits struct {
	// CPUSeconds 限制单个进程的 CPU 时间（RLIMIT_CPU / ulimit -t）。
	CPUSeconds int `yaml:"cpu_seconds"`
	// MemoryMB 限制地址空间（RLIMIT_AS / ulimit -v）。
	MemoryMB int `yaml:"memory_mb"`
	// MaxFiles 限制打开的文件句柄数（RLIMIT_NOFILE / ulimit -n）。
	MaxFiles int `yaml:"max_files"`
}

// 执行默认值。EventLimit 刻意压在 agent 的 8 KB 阈值上：超出就会被二次
// 截断成 6 KB，等于白留一份没用的中段。
const (
	DefaultExecBackend  = "direct"
	SandboxExecBackend  = "sandbox"
	DefaultEventLimit   = 8 << 10
	DefaultExecLogLimit = 4 << 20
	// MaxExecEventLimit 对齐 agent 的 8 KB 工具结果阈值：再大也会被二次
	// 截成首 4 KB + 尾 2 KB，等于白留一段永远到不了模型的中段。
	MaxExecEventLimit     = 8 << 10
	MaxExecTimeoutSeconds = 600
)

// Server 是监听与鉴权配置。
type Server struct {
	Listen string `yaml:"listen"`
	// Token 是单个访问令牌，等同于 tokens 里一条名为 default 的可写令牌。
	Token Secret `yaml:"token"`
	// Tokens 是命名令牌列表。与 Token 可同时存在；只读令牌只能看，不能改。
	Tokens []ServerToken `yaml:"tokens"`
	// TLSCert / TLSKey 同时配置才启用 TLS（wss）。只配一个会报错，
	// 避免出现「以为加密了、其实是明文」的部署。
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
}

// ServerToken 是一个命名访问令牌。
//
// ReadOnly 的令牌只能读：订阅事件、拉审计、列会话、查模型与 MCP 状态；
// 提交任务、裁决审批、改会话、信任项目一律拒绝。导出工具该用它。
type ServerToken struct {
	Name  string `yaml:"name"`
	Value Secret `yaml:"value"`
	// ReadOnly 为 true 时禁止一切写操作。默认 false（与历史行为一致）。
	ReadOnly bool `yaml:"read_only"`
}

// AllTokens 把 token 与 tokens 合并成一份列表：token 视作名为 default 的可写令牌。
func (s Server) AllTokens() []ServerToken {
	var out []ServerToken
	if s.Token != "" {
		out = append(out, ServerToken{Name: "default", Value: s.Token})
	}
	out = append(out, s.Tokens...)
	return out
}

// ClientToken 返回客户端默认使用的令牌：优先 server.token，否则第一个可写令牌。
// 只配置了只读令牌时返回错误——那台服务端不接受写操作，客户端拿它没有意义。
func (s Server) ClientToken() (Secret, error) {
	if s.Token != "" {
		return s.Token, nil
	}
	for _, t := range s.Tokens {
		if !t.ReadOnly {
			return t.Value, nil
		}
	}
	return "", errors.New("server.token 未配置，且 server.tokens 里没有可写令牌")
}

// TLSEnabled 报告是否配置了证书对。
func (s Server) TLSEnabled() bool { return s.TLSCert != "" && s.TLSKey != "" }

// validateTLS 校验证书对：两个都配或都不配，且能真正加载。
//
// 启动时就解析，是为了把「证书路径写错」挡在监听之前——否则错误要等
// 第一个连接进来才暴露，看着像客户端问题。
func (c *Config) validateTLS() error {
	cert, key := strings.TrimSpace(c.Server.TLSCert), strings.TrimSpace(c.Server.TLSKey)
	switch {
	case cert == "" && key == "":
		return nil
	case cert == "" || key == "":
		return errors.New("server.tls_cert 与 server.tls_key 必须同时配置")
	}
	certPath, err := ExpandHome(cert)
	if err != nil {
		return fmt.Errorf("server.tls_cert: %w", err)
	}
	keyPath, err := ExpandHome(key)
	if err != nil {
		return fmt.Errorf("server.tls_key: %w", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("server.tls_cert/tls_key 无法加载: %w", err)
	}
	c.Server.TLSCert, c.Server.TLSKey = certPath, keyPath
	return nil
}

// Storage 是持久化位置。
type Storage struct {
	DBPath    string `yaml:"db_path"`
	OutputDir string `yaml:"output_dir"`
}

// Agent 是自主循环的执行约束。
type Agent struct {
	// MaxRounds 是单任务最多轮数；0 表示不限制，留空用默认值 24。
	MaxRounds             *int `yaml:"max_rounds"`
	CommandTimeoutSeconds int  `yaml:"command_timeout_seconds"`
	// ContextBudgetTokens 是上下文预算（估算 token 数），超出的最旧轮次会被
	// 丢弃。估算见 internal/agent/tokens.go：按字符类别加权，并用供应商回报的
	// 实际 prompt_tokens 在线校准。
	ContextBudgetTokens int `yaml:"context_budget_tokens"`
	// ContextBudgetChars 是 v2 早期的预算写法，保留兼容：未配置
	// context_budget_tokens 时按 2 字符/token 折算，两个都配置以 token 为准。
	ContextBudgetChars int `yaml:"context_budget_chars"`
	// ContextCompact 控制上下文压缩：auto（默认）超出预算 80% 时把最旧轮次
	// 压成摘要并保留最近 2 轮；off 关闭，只做丢弃。
	ContextCompact string `yaml:"context_compact"`
	// MaxConcurrentTasks 是跨会话同时执行的任务数上限；0 表示不限。
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks"`
	// MaxQueueDepth 是单会话排队任务数上限；0 表示不限。
	MaxQueueDepth int `yaml:"max_queue_depth"`
	// ResumeQueued 控制服务端重启后是否继续执行排队中的任务。
	// 默认开启；执行中/等审批的任务仍按失败收尾，不会自动重跑。
	ResumeQueued *bool `yaml:"resume_queued"`
	// ApprovalTTL 是 allow_session 授权的有效期（如 12h、30m），默认 12h。
	// 授权落库、跨重启生效，但过期后同一操作会重新询问。
	ApprovalTTL string `yaml:"approval_ttl"`
	// Memory 控制跨会话项目记忆：auto（默认）注入工作区 MEMORY.md 并注册
	// remember 工具；off 关闭，既不注入也不提供写入工具。
	Memory string `yaml:"memory"`
	// Skills 控制技能包：auto（默认）把索引注入系统提示并注册 skill 工具，
	// 正文按需加载；off 关闭。
	Skills string `yaml:"skills"`
	// SkillsDirs 是技能包目录；留空用 DefaultSkillsDir。`~` 会在启动时展开，
	// 相对路径按会话工作区根解析。
	SkillsDirs []string `yaml:"skills_dirs"`
}

// MemoryEnabled 报告是否启用跨会话项目记忆；未配置时默认开启。
func (a Agent) MemoryEnabled() bool { return a.Memory != MemoryOff }

// SkillDirsOrDefault 返回技能包目录；未配置时用 DefaultSkillsDir。
func (a Agent) SkillDirsOrDefault() []string {
	if len(a.SkillsDirs) == 0 {
		return []string{DefaultSkillsDir}
	}
	return a.SkillsDirs
}

// SkillDirsFor 返回生效的技能目录：skills 为 off 时为空，连 /skill 面板也不列。
func (a Agent) SkillDirsFor() []string {
	if a.Skills == SkillOff {
		return nil
	}
	return a.SkillDirsOrDefault()
}

// ApprovalTTLDuration 把 agent.approval_ttl 解析成时长；配置已在 Validate
// 阶段校验过，这里解析失败时退回默认值，避免把无效值传成 0。
func (a Agent) ApprovalTTLDuration() time.Duration {
	if ttl, err := time.ParseDuration(a.ApprovalTTL); err == nil && ttl > 0 {
		return ttl
	}
	fallback, _ := time.ParseDuration(DefaultApprovalTTL)
	return fallback
}

// ResumeQueuedEnabled 报告重启后是否续跑排队任务；未配置时默认开启。
func (a Agent) ResumeQueuedEnabled() bool {
	return a.ResumeQueued == nil || *a.ResumeQueued
}

// DefaultContextBudgetTokens 与 agent.DefaultContextBudget 保持一致（估算 token）。
const DefaultContextBudgetTokens = 32000

// DefaultContextBudgetChars 是旧的字符预算默认值，只在兼容路径上使用。
const DefaultContextBudgetChars = 60000

// charsPerToken 是旧配置折算 token 时用的比例：中文约 1 字/token、英文约
// 4 字符/token，取中间值，具体偏差交给运行时校准吸收。
const charsPerToken = 2

// ContextBudgetTokenValue 返回实际生效的 token 预算。未配置 token 预算时把
// 旧的字符预算按 charsPerToken 折算；两者都没配则用默认值。
func (a Agent) ContextBudgetTokenValue() int {
	if a.ContextBudgetTokens > 0 {
		return a.ContextBudgetTokens
	}
	if a.ContextBudgetChars > 0 {
		return a.ContextBudgetChars / charsPerToken
	}
	return DefaultContextBudgetTokens
}

// ContextCompactAuto 是上下文压缩的默认模式。
const ContextCompactAuto = "auto"

// MemoryAuto 是跨会话记忆的默认模式；MemoryOff 关闭注入与 remember 工具。
const (
	MemoryAuto = "auto"
	MemoryOff  = "off"
)

// SkillAuto 是技能包的默认模式；SkillOff 关闭索引注入与 skill 工具。
const (
	SkillAuto        = "auto"
	SkillOff         = "off"
	DefaultSkillsDir = "~/.agents/skills"
)

// DefaultMaxRounds 是未配置 agent.max_rounds 时的轮数上限。
const DefaultMaxRounds = 24

// DefaultApprovalTTL 是未配置 agent.approval_ttl 时 allow_session 授权的有效期。
const DefaultApprovalTTL = "12h"

// MaxRoundsValue 返回生效的轮数上限；0 表示不限制。
func (a Agent) MaxRoundsValue() int {
	if a.MaxRounds == nil {
		return DefaultMaxRounds
	}
	return *a.MaxRounds
}

// DefaultMaxConcurrentTasks 是跨会话同时执行的任务数默认上限。
const DefaultMaxConcurrentTasks = 4

// DefaultMaxQueueDepth 是单会话排队任务数的默认上限。
const DefaultMaxQueueDepth = 16

// Provider 描述一家模型供应商。
type Provider struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // openai | anthropic
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	// Models 是 /models 的候选列表。非空时直接展示它，不再请求供应商的
	// /models 接口；适合网关不实现列表、或只想暴露少数模型的场景。
	Models []string `yaml:"models"`
	// APIKey 是单 key 写法，保留兼容。APIKeys 非空时优先用列表。
	APIKey Secret `yaml:"api_key"`
	// APIKeys 是同一供应商的多把 key：请求轮询取用，遇到鉴权 / 额度 /
	// 限流失败时把该 key 冷却一段时间并换下一把。元素写法与 api_key 相同
	// （明文 / $ENV_NAME / file:/path）。
	APIKeys []Secret `yaml:"api_keys"`

	// TimeoutSeconds 与 MaxRetries 用指针区分「未配置」与「显式 0」：
	// 未配置走默认值，显式 0 表示不重试（合法），所以不能让零值等价于缺省。
	TimeoutSeconds *int `yaml:"timeout_seconds"`
	MaxRetries     *int `yaml:"max_retries"`

	// RPM 是每分钟请求数上限；0 表示不限。超出的请求会等待而不是失败，
	// 避免把 429 的重试放大成雪崩。
	RPM int `yaml:"rpm"`
	// TPM 是每分钟 token 数上限（输入+输出估算）；0 表示不限。
	TPM int `yaml:"tpm"`
}

// 供应商调用的默认约束。
const (
	DefaultProviderTimeoutSeconds = 120
	DefaultProviderMaxRetries     = 2
)

// Timeout 返回单次尝试的首块超时，未配置时用默认值。
func (p Provider) Timeout() time.Duration {
	seconds := DefaultProviderTimeoutSeconds
	if p.TimeoutSeconds != nil {
		seconds = *p.TimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// Retries 返回重试次数（不含第一次），未配置时用默认值。
func (p Provider) Retries() int {
	if p.MaxRetries != nil {
		return *p.MaxRetries
	}
	return DefaultProviderMaxRetries
}

// Secret 是一段延迟解析的敏感值。实现 String 以避免被日志或 %v 打印出明文。
type Secret string

// String 永远不返回明文。
func (s Secret) String() string { return "<已隐藏>" }

// Resolve 把三种写法解析成真实值。
func (s Secret) Resolve() (string, error) {
	raw := strings.TrimSpace(string(s))
	switch {
	case raw == "":
		return "", errors.New("未配置")
	case strings.HasPrefix(raw, "$"):
		name := strings.TrimPrefix(raw, "$")
		if name == "" {
			return "", errors.New("环境变量名为空")
		}
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return "", fmt.Errorf("环境变量 %s 未设置", name)
		}
		return v, nil
	case strings.HasPrefix(raw, "file:"):
		p, err := ExpandHome(strings.TrimPrefix(raw, "file:"))
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("读取密钥文件 %s 失败: %w", p, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("密钥文件 %s 为空", p)
		}
		return v, nil
	default:
		return raw, nil
	}
}

// ExpandHome 展开开头的 ~。
func ExpandHome(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户主目录: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// ConfigDir 返回配置目录，遵循 XDG_CONFIG_HOME。
func ConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, AppName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "." + AppName
	}
	return filepath.Join(home, ".config", AppName)
}

// DataDir 返回数据目录，遵循 XDG_DATA_HOME。
func DataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, AppName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "." + AppName
	}
	return filepath.Join(home, ".local", "share", AppName)
}

// DefaultPath 返回默认配置文件路径。
func DefaultPath() string { return filepath.Join(ConfigDir(), "config.yaml") }

// Load 读取并解析配置，但不校验内容——服务端与客户端校验的字段不同。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	p, err := ExpandHome(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("配置文件不存在：%s\n先运行 `jiaoag-server -init` 生成样例，再按注释填写", p)
		}
		return nil, fmt.Errorf("读取配置 %s 失败: %w", p, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", p, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8787"
	}
	if c.Storage.DBPath == "" {
		c.Storage.DBPath = filepath.Join(DataDir(), "jiaoag.db")
	}
	if c.Storage.OutputDir == "" {
		c.Storage.OutputDir = filepath.Join(DataDir(), "outputs")
	}
	if c.Agent.MaxRounds == nil {
		rounds := DefaultMaxRounds
		c.Agent.MaxRounds = &rounds
	}
	if c.Agent.CommandTimeoutSeconds <= 0 {
		c.Agent.CommandTimeoutSeconds = 120
	}
	if c.Agent.ContextBudgetTokens <= 0 && c.Agent.ContextBudgetChars <= 0 {
		c.Agent.ContextBudgetTokens = DefaultContextBudgetTokens
	}
	if c.Agent.ContextCompact == "" {
		c.Agent.ContextCompact = ContextCompactAuto
	}
	// 0 表示「用默认值」，不提供真正的无限；确需放开就填一个很大的数。
	if c.Agent.MaxConcurrentTasks <= 0 {
		c.Agent.MaxConcurrentTasks = DefaultMaxConcurrentTasks
	}
	if c.Agent.MaxQueueDepth <= 0 {
		c.Agent.MaxQueueDepth = DefaultMaxQueueDepth
	}
	if c.Agent.ResumeQueued == nil {
		enabled := true
		c.Agent.ResumeQueued = &enabled
	}
	if c.Agent.ApprovalTTL == "" {
		c.Agent.ApprovalTTL = DefaultApprovalTTL
	}
	if c.Agent.Memory == "" {
		c.Agent.Memory = MemoryAuto
	}
	if c.Agent.Skills == "" {
		c.Agent.Skills = SkillAuto
	}
	if c.Project.Mode == "" {
		c.Project.Mode = ProjectAsk
	}
	for i, dir := range c.Agent.SkillsDirs {
		// 技能目录允许 ~，展开后再落进运行时；相对路径保持原样，
		// 由各会话按自己的工作区根解析。
		if expanded, err := ExpandHome(strings.TrimSpace(dir)); err == nil {
			c.Agent.SkillsDirs[i] = expanded
		}
	}
	if c.Policy.Dir == "" {
		c.Policy.Dir = filepath.Join(ConfigDir(), "policy.d")
	}
	if c.Exec.Backend == "" {
		c.Exec.Backend = DefaultExecBackend
	}
	if c.Exec.TimeoutSeconds <= 0 {
		c.Exec.TimeoutSeconds = c.Agent.CommandTimeoutSeconds
	}
	if c.Exec.EventLimit <= 0 {
		c.Exec.EventLimit = DefaultEventLimit
	}
	if c.Exec.LogLimit <= 0 {
		c.Exec.LogLimit = DefaultExecLogLimit
	}
	for i := range c.MCP.Servers {
		s := &c.MCP.Servers[i]
		if s.TimeoutSeconds <= 0 {
			s.TimeoutSeconds = 60
		}
		if s.Trust == "" {
			s.Trust = "allow"
		}
	}
}

// validateMCP 校验外部工具配置。工具名会进入系统提示与审计，非法名必须
// 在启动时挡住，而不是等到模型调用时才失败。
func (c *Config) validateMCP() error {
	seen := make(map[string]bool, len(c.MCP.Servers))
	for i, s := range c.MCP.Servers {
		prefix := fmt.Sprintf("mcp.servers[%d]", i)
		if s.Name == "" {
			return fmt.Errorf("%s.name 不能为空", prefix)
		}
		if !validMCPName(s.Name) {
			return fmt.Errorf("%s.name 只允许字母、数字、'_'、'-'，当前为 %q", prefix, s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s.name 重复：%s", prefix, s.Name)
		}
		seen[s.Name] = true
		if s.Command == "" {
			return fmt.Errorf("%s.command 不能为空", prefix)
		}
		switch s.Trust {
		case "ask", "allow":
		default:
			return fmt.Errorf("%s.trust 必须是 ask 或 allow，当前为 %q", prefix, s.Trust)
		}
		if s.TimeoutSeconds < 0 || s.TimeoutSeconds > 600 {
			return fmt.Errorf("%s.timeout_s 必须在 0–600 之间，当前为 %d", prefix, s.TimeoutSeconds)
		}
		for _, list := range []struct {
			key   string
			items []string
		}{
			{"include", s.Tools.Include},
			{"exclude", s.Tools.Exclude},
		} {
			for j, name := range list.items {
				if strings.TrimSpace(name) == "" {
					return fmt.Errorf("%s.tools.%s[%d] 不能为空", prefix, list.key, j)
				}
			}
		}
		if s.Dir != "" {
			if _, err := ExpandHome(s.Dir); err != nil {
				return fmt.Errorf("%s.dir: %w", prefix, err)
			}
		}
	}
	return nil
}

func validMCPName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateServer 校验服务端启动所需的全部字段。
func (c *Config) ValidateServer() error {
	if err := c.validateCommon(); err != nil {
		return err
	}
	if err := c.validateMCP(); err != nil {
		return err
	}
	if len(c.Providers) == 0 {
		return errors.New("至少需要一个 providers 条目")
	}
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		prefix := fmt.Sprintf("providers[%d]", i)
		if p.Name == "" {
			return fmt.Errorf("%s.name 不能为空", prefix)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s.name 重复：%s", prefix, p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case "openai", "anthropic":
		default:
			return fmt.Errorf("%s.type 必须是 openai 或 anthropic，当前为 %q", prefix, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("%s.base_url 不能为空", prefix)
		}
		if p.Model == "" {
			return fmt.Errorf("%s.model 不能为空", prefix)
		}
		if _, err := p.APIKey.Resolve(); p.APIKey != "" && err != nil {
			return fmt.Errorf("%s.api_key: %w（支持明文 / $ENV_NAME / file:/path）", prefix, err)
		}
		if len(p.APIKeys) == 0 && p.APIKey == "" {
			return fmt.Errorf("%s.api_key 或 api_keys 至少要配置一个", prefix)
		}
		for j, key := range p.APIKeys {
			if _, err := key.Resolve(); err != nil {
				return fmt.Errorf("%s.api_keys[%d]: %w（支持明文 / $ENV_NAME / file:/path）", prefix, j, err)
			}
		}
		if p.TimeoutSeconds != nil && *p.TimeoutSeconds <= 0 {
			return fmt.Errorf("%s.timeout_seconds 必须大于 0，当前为 %d", prefix, *p.TimeoutSeconds)
		}
		if p.MaxRetries != nil && *p.MaxRetries < 0 {
			return fmt.Errorf("%s.max_retries 不能为负数，当前为 %d", prefix, *p.MaxRetries)
		}
	}
	return nil
}

// ValidateClient 只校验客户端连接所需字段：客户端不需要 provider 的密钥。
func (c *Config) ValidateClient() error {
	if err := c.validateCommon(); err != nil {
		return err
	}
	token, err := c.Server.ClientToken()
	if err != nil {
		return fmt.Errorf("server.token: %w", err)
	}
	if _, err := token.Resolve(); err != nil {
		return fmt.Errorf("server.token: %w（支持明文 / $ENV_NAME / file:/path）", err)
	}
	return nil
}

// validateTokens 校验 token 与 tokens 的形状，并逐个解析取值。
// 启动时的解析失败要立刻报错，否则服务端会带着一个永远认证不上的令牌起来。
// validatePolicyFeed 校验签名规则 feed 的配置。
//
// url 与 keys 要么都不配（不用 feed），要么都配：只配一半会让「我以为有签名
// 保护」变成静默落空，比完全不用 feed 更危险。
func (c *Config) validatePolicyFeed() error {
	url := strings.TrimSpace(c.Policy.Feed.URL)
	configured := 0
	for i, key := range c.Policy.Feed.Keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("policy.feed.keys[%d] 不能为空", i)
		}
		configured++
	}
	if url == "" && configured == 0 {
		return nil
	}
	if url == "" {
		return errors.New("policy.feed.url 不能为空（已配置公钥）")
	}
	if configured == 0 {
		return errors.New("policy.feed.keys 至少要配置一个公钥（已配置 url）")
	}
	if _, err := policyfeed.ParseKeys(c.Policy.Feed.Keys); err != nil {
		return fmt.Errorf("policy.feed.keys: %w", err)
	}
	return nil
}

// FeedKeys 返回解析后的 feed 公钥；未配置时返回 nil。
func (c *Config) FeedKeys() (policyfeed.Keys, error) {
	if len(c.Policy.Feed.Keys) == 0 {
		return nil, nil
	}
	return policyfeed.ParseKeys(c.Policy.Feed.Keys)
}

func (c *Config) validateTokens() error {
	seen := make(map[string]bool, len(c.Server.Tokens)+1)
	total := 0
	if c.Server.Token != "" {
		if _, err := c.Server.Token.Resolve(); err != nil {
			return fmt.Errorf("server.token: %w（支持明文 / $ENV_NAME / file:/path）", err)
		}
		seen["default"] = true
		total++
	}
	for i, t := range c.Server.Tokens {
		prefix := fmt.Sprintf("server.tokens[%d]", i)
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return fmt.Errorf("%s.name 不能为空", prefix)
		}
		if len(name) > 32 {
			return fmt.Errorf("%s.name 过长（最多 32 个字符）：%s", prefix, name)
		}
		for _, r := range name {
			ok := r == '_' || r == '-' || r == '.' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				return fmt.Errorf("%s.name 只能包含字母、数字、_ - .，当前为 %q", prefix, name)
			}
		}
		if seen[name] {
			return fmt.Errorf("%s.name 重复：%s", prefix, name)
		}
		seen[name] = true
		if _, err := t.Value.Resolve(); err != nil {
			return fmt.Errorf("%s.value: %w（支持明文 / $ENV_NAME / file:/path）", prefix, err)
		}
		total++
	}
	if total == 0 {
		return errors.New("server.token 或 server.tokens 至少要配置一个")
	}
	return nil
}

func (c *Config) validateCommon() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen 不能为空")
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	if c.Agent.ContextBudgetTokens > 0 && c.Agent.ContextBudgetTokens < 1000 {
		return fmt.Errorf("agent.context_budget_tokens 必须为 0 或大于 1000，当前为 %d", c.Agent.ContextBudgetTokens)
	}
	if c.Agent.ContextBudgetChars > 0 && c.Agent.ContextBudgetChars <= 1000 {
		return fmt.Errorf("agent.context_budget_chars 必须大于 1000，当前为 %d", c.Agent.ContextBudgetChars)
	}
	switch c.Agent.ContextCompact {
	case "", ContextCompactAuto, "off":
	default:
		return fmt.Errorf("agent.context_compact 只支持 %q 或 %q，当前为 %q", ContextCompactAuto, "off", c.Agent.ContextCompact)
	}
	switch c.Agent.Memory {
	case "", MemoryAuto, MemoryOff:
	default:
		return fmt.Errorf("agent.memory 只支持 %q 或 %q，当前为 %q", MemoryAuto, MemoryOff, c.Agent.Memory)
	}
	// 空串表示「用默认值」：测试与嵌入式调用会直接构造 Config，不经过
	// applyDefaults，此时不该把缺省当成非法。
	if c.Agent.ApprovalTTL != "" {
		if ttl, err := time.ParseDuration(c.Agent.ApprovalTTL); err != nil || ttl <= 0 {
			return fmt.Errorf("agent.approval_ttl 必须是正的时长（如 12h、30m），当前为 %q", c.Agent.ApprovalTTL)
		}
	}
	switch c.Agent.Skills {
	case "", SkillAuto, SkillOff:
	default:
		return fmt.Errorf("agent.skills 只支持 %q 或 %q，当前为 %q", SkillAuto, SkillOff, c.Agent.Skills)
	}
	for i, dir := range c.Agent.SkillsDirs {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("agent.skills_dirs[%d] 不能为空", i)
		}
	}
	switch c.Project.Mode {
	case "", ProjectAsk, ProjectAuto, ProjectOff:
	default:
		return fmt.Errorf("project.mode 只支持 %q / %q / %q，当前为 %q", ProjectAsk, ProjectAuto, ProjectOff, c.Project.Mode)
	}
	if err := c.validateTokens(); err != nil {
		return err
	}
	if err := c.validatePolicyFeed(); err != nil {
		return err
	}
	for name, p := range map[string]string{"storage.db_path": c.Storage.DBPath, "storage.output_dir": c.Storage.OutputDir} {
		if p == "" {
			return fmt.Errorf("%s 不能为空", name)
		}
	}
	// 配置里的 ~ 必须在启动时展开：否则相对当前目录会建出一个字面量
	// 「~」目录，数据库看着像丢了，其实落在启动目录下。
	if p, err := ExpandHome(c.Storage.DBPath); err != nil {
		return fmt.Errorf("storage.db_path: %w", err)
	} else {
		c.Storage.DBPath = p
	}
	if p, err := ExpandHome(c.Storage.OutputDir); err != nil {
		return fmt.Errorf("storage.output_dir: %w", err)
	} else {
		c.Storage.OutputDir = p
	}
	if c.Agent.MaxRounds != nil && *c.Agent.MaxRounds < 0 {
		return fmt.Errorf("agent.max_rounds 不能为负数（0 表示不限制），当前为 %d", *c.Agent.MaxRounds)
	}
	switch c.Exec.Backend {
	case DefaultExecBackend, SandboxExecBackend:
	default:
		return fmt.Errorf("exec.backend 只支持 %q 或 %q，当前为 %q", DefaultExecBackend, SandboxExecBackend, c.Exec.Backend)
	}
	if c.Exec.TimeoutSeconds <= 0 || c.Exec.TimeoutSeconds > MaxExecTimeoutSeconds {
		return fmt.Errorf("exec.timeout_s 必须在 1-%d 之间，当前为 %d", MaxExecTimeoutSeconds, c.Exec.TimeoutSeconds)
	}
	if c.Exec.EventLimit < 1024 || c.Exec.EventLimit > MaxExecEventLimit {
		return fmt.Errorf("exec.event_limit 必须在 1024-%d 之间（不超过 8 KB 工具结果阈值），当前为 %d", MaxExecEventLimit, c.Exec.EventLimit)
	}
	if c.Exec.LogLimit < c.Exec.EventLimit {
		return fmt.Errorf("exec.log_limit(%d) 不能小于 exec.event_limit(%d)", c.Exec.LogLimit, c.Exec.EventLimit)
	}
	switch {
	case c.Exec.Limits.CPUSeconds < 0:
		return errors.New("exec.limits.cpu_seconds 不能为负")
	case c.Exec.Limits.MemoryMB < 0:
		return errors.New("exec.limits.memory_mb 不能为负")
	case c.Exec.Limits.MaxFiles < 0:
		return errors.New("exec.limits.max_files 不能为负")
	}
	if mb := c.Exec.Limits.MemoryMB; mb > 0 && mb < MinExecMemoryMB {
		return fmt.Errorf("exec.limits.memory_mb(%d) 太小，至少 %d", mb, MinExecMemoryMB)
	}
	// 负数没有意义：上限要么不设（0 走默认），要么是正数。
	if c.Agent.MaxConcurrentTasks < 0 {
		return errors.New("agent.max_concurrent_tasks 不能为负")
	}
	if c.Agent.MaxQueueDepth < 0 {
		return errors.New("agent.max_queue_depth 不能为负")
	}
	for _, p := range c.Providers {
		if p.RPM < 0 || p.TPM < 0 {
			return fmt.Errorf("providers[%s] 的 rpm/tpm 不能为负", p.Name)
		}
	}
	return nil
}

// Provider 按名字查找供应商。
func (c *Config) Provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// DefaultProvider 返回第一个供应商，作为未指定 provider 时的默认。
func (c *Config) DefaultProvider() (Provider, bool) {
	if len(c.Providers) == 0 {
		return Provider{}, false
	}
	return c.Providers[0], true
}

// WriteExample 把样例配置写到 path；已存在时不覆盖。
func WriteExample(path string) error {
	if path == "" {
		path = DefaultPath()
	}
	p, err := ExpandHome(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("配置文件已存在，未覆盖：%s", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.WriteFile(p, exampleYAML, 0o600); err != nil {
		return fmt.Errorf("写入样例配置失败: %w", err)
	}
	return nil
}
