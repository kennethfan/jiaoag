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
	Providers []Provider `yaml:"providers"`
}

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
}

// Provider 描述一家模型供应商。
type Provider struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // openai | anthropic
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	APIKey  Secret `yaml:"api_key"`
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
	}
	return nil
}

// ValidateClient 只校验客户端连接所需字段：客户端不需要 provider 的密钥。
func (c *Config) ValidateClient() error { return c.validateCommon() }

func (c *Config) validateCommon() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen 不能为空")
	}
	if _, err := c.Server.Token.Resolve(); err != nil {
		return fmt.Errorf("server.token: %w（支持明文 / $ENV_NAME / file:/path）", err)
	}
	for name, p := range map[string]string{"storage.db_path": c.Storage.DBPath, "storage.output_dir": c.Storage.OutputDir} {
		if p == "" {
			return fmt.Errorf("%s 不能为空", name)
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
