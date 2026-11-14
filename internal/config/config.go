// Package config 负责加载配置并解析密钥来源。
//
// 配置默认位于 ~/.config/jiaoag/config.yaml；所有路径支持 ~ 展开，
// 所有密钥支持三种写法：明文、$ENV_NAME、file:/abs/path。
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
}

// Policy 是本地权限规则的位置。外部规则只能收紧（ADR-0010）。
type Policy struct {
	Dir string `yaml:"dir"`
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
	Token  Secret `yaml:"token"`
}

// Storage 是持久化位置。
type Storage struct {
	DBPath    string `yaml:"db_path"`
	OutputDir string `yaml:"output_dir"`
}

// Agent 是自主循环的执行约束。
type Agent struct {
	MaxRounds             int `yaml:"max_rounds"`
	CommandTimeoutSeconds int `yaml:"command_timeout_seconds"`
	// ContextBudgetChars 是上下文预算（UTF-8 字符数），超出的最旧轮次会被丢弃。
	ContextBudgetChars int `yaml:"context_budget_chars"`
}

// DefaultContextBudgetChars 与 agent.DefaultContextBudget 保持一致。
const DefaultContextBudgetChars = 60000

// Provider 描述一家模型供应商。
type Provider struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // openai | anthropic
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	APIKey  Secret `yaml:"api_key"`

	// TimeoutSeconds 与 MaxRetries 用指针区分「未配置」与「显式 0」：
	// 未配置走默认值，显式 0 表示不重试（合法），所以不能让零值等价于缺省。
	TimeoutSeconds *int `yaml:"timeout_seconds"`
	MaxRetries     *int `yaml:"max_retries"`
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
	if c.Agent.MaxRounds <= 0 {
		c.Agent.MaxRounds = 24
	}
	if c.Agent.CommandTimeoutSeconds <= 0 {
		c.Agent.CommandTimeoutSeconds = 120
	}
	if c.Agent.ContextBudgetChars <= 0 {
		c.Agent.ContextBudgetChars = DefaultContextBudgetChars
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
}

// ValidateServer 校验服务端启动所需的全部字段。
func (c *Config) ValidateServer() error {
	if err := c.validateCommon(); err != nil {
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
		if _, err := p.APIKey.Resolve(); err != nil {
			return fmt.Errorf("%s.api_key: %w（支持明文 / $ENV_NAME / file:/path）", prefix, err)
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
func (c *Config) ValidateClient() error { return c.validateCommon() }

func (c *Config) validateCommon() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen 不能为空")
	}
	if c.Agent.ContextBudgetChars > 0 && c.Agent.ContextBudgetChars <= 1000 {
		return fmt.Errorf("agent.context_budget_chars 必须大于 1000，当前为 %d", c.Agent.ContextBudgetChars)
	}
	if _, err := c.Server.Token.Resolve(); err != nil {
		return fmt.Errorf("server.token: %w（支持明文 / $ENV_NAME / file:/path）", err)
	}
	for name, p := range map[string]string{"storage.db_path": c.Storage.DBPath, "storage.output_dir": c.Storage.OutputDir} {
		if p == "" {
			return fmt.Errorf("%s 不能为空", name)
		}
	}
	if c.Exec.Backend != DefaultExecBackend {
		return fmt.Errorf("exec.backend 目前只支持 %q，当前为 %q", DefaultExecBackend, c.Exec.Backend)
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
