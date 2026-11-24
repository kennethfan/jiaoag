package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setHome 把用户主目录指到临时目录，让 ~ 展开可断言。
//
// Windows 上 os.UserHomeDir 读的是 USERPROFILE 而不是 HOME，只设 HOME 时
// 测试会拿真实主目录去比较，CI 里必挂。
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	if runtime.GOOS != "windows" {
		return
	}
	t.Setenv("USERPROFILE", dir)
	volume := filepath.VolumeName(dir)
	t.Setenv("HOMEDRIVE", volume)
	t.Setenv("HOMEPATH", strings.TrimPrefix(dir, volume))
}

const minimalYAML = `server:
  listen: 127.0.0.1:9999
  token: literal-token
providers:
  - name: p
    type: openai
    base_url: http://127.0.0.1:1/v1
    model: m
    api_key: k
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}
	return p
}

func TestLoadMissingFileIsActionable(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "配置文件不存在") || !strings.Contains(err.Error(), "-init") {
		t.Fatalf("错误信息不可操作: %v", err)
	}
}

func TestMaxRoundsZeroMeansUnlimited(t *testing.T) {
	cfg, err := Load(writeTemp(t, strings.Replace(minimalYAML, "providers:", "agent:\n  max_rounds: 0\nproviders:", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.MaxRoundsValue(); got != 0 {
		t.Fatalf("显式 0 应保留为「不限制」，实际 %d", got)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("0 应通过校验：%v", err)
	}

	cfg, err = Load(writeTemp(t, strings.Replace(minimalYAML, "providers:", "agent:\n  max_rounds: -1\nproviders:", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.ValidateServer()
	if err == nil || !strings.Contains(err.Error(), "不能为负数") {
		t.Fatalf("负数应报错，得到 %v", err)
	}
}

func TestStoragePathsExpandHome(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	yaml := strings.Replace(minimalYAML, "providers:",
		"storage:\n  db_path: ~/data/jiaoag.db\n  output_dir: ~/out\nproviders:", 1)
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
	if want := filepath.Join(home, "data", "jiaoag.db"); cfg.Storage.DBPath != want {
		t.Fatalf("db_path = %q，期望 %q", cfg.Storage.DBPath, want)
	}
	if want := filepath.Join(home, "out"); cfg.Storage.OutputDir != want {
		t.Fatalf("output_dir = %q，期望 %q", cfg.Storage.OutputDir, want)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.MaxRoundsValue() != 24 {
		t.Errorf("MaxRounds = %d，期望 24", cfg.Agent.MaxRoundsValue())
	}
	if cfg.Agent.CommandTimeoutSeconds != 120 {
		t.Errorf("CommandTimeoutSeconds = %d，期望 120", cfg.Agent.CommandTimeoutSeconds)
	}
	if !strings.HasSuffix(cfg.Storage.DBPath, "jiaoag.db") {
		t.Errorf("DBPath = %q，期望以 jiaoag.db 结尾", cfg.Storage.DBPath)
	}
	if !strings.HasSuffix(cfg.Storage.OutputDir, "outputs") {
		t.Errorf("OutputDir = %q，期望以 outputs 结尾", cfg.Storage.OutputDir)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
}

func TestValidateServerRequiresProviders(t *testing.T) {
	cfg, err := Load(writeTemp(t, "server:\n  listen: 127.0.0.1:1\n  token: t\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("期望 providers 缺失错误，得到 %v", err)
	}
	if err := cfg.ValidateClient(); err != nil {
		t.Fatalf("客户端不应要求 providers: %v", err)
	}
}

func TestValidateServerRejectsUnknownProviderType(t *testing.T) {
	cfg, err := Load(writeTemp(t, strings.Replace(minimalYAML, "type: openai", "type: gemini", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.ValidateServer()
	if err == nil || !strings.Contains(err.Error(), "type 必须是") {
		t.Fatalf("期望 type 校验错误，得到 %v", err)
	}
}

func TestValidateServerRejectsUnresolvableKey(t *testing.T) {
	cfg, err := Load(writeTemp(t, strings.Replace(minimalYAML, "api_key: k", "api_key: $JIAOAG_DEFINITELY_UNSET", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.ValidateServer()
	if err == nil || !strings.Contains(err.Error(), "环境变量 JIAOAG_DEFINITELY_UNSET 未设置") {
		t.Fatalf("期望环境变量错误，得到 %v", err)
	}
}

func TestValidateServerAPIKeys(t *testing.T) {
	cfg, err := Load(writeTemp(t, strings.Replace(minimalYAML, "api_key: k", "api_keys:\n      - k1\n      - k2", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("多 key 配置应通过校验：%v", err)
	}
	if len(cfg.Providers[0].APIKeys) != 2 {
		t.Fatalf("api_keys 未解析：%+v", cfg.Providers[0].APIKeys)
	}

	// 列表里任何一个 key 解析失败都要指出下标，且不回显明文。
	cfg, err = Load(writeTemp(t, strings.Replace(minimalYAML, "api_key: k", "api_keys:\n      - k1\n      - $JIAOAG_NO_SUCH_KEY", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.ValidateServer()
	if err == nil || !strings.Contains(err.Error(), "api_keys[1]") {
		t.Fatalf("期望指出 api_keys[1] 的错误，得到 %v", err)
	}

	// api_key 与 api_keys 都不配是配置错误，不能等到第一次请求才暴露。
	cfg, err = Load(writeTemp(t, strings.Replace(minimalYAML, "    api_key: k\n", "", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.ValidateServer()
	if err == nil || !strings.Contains(err.Error(), "至少要配置一个") {
		t.Fatalf("期望缺少 key 的校验错误，得到 %v", err)
	}
}

func TestSecretResolve(t *testing.T) {
	t.Run("明文", func(t *testing.T) {
		got, err := Secret("plain").Resolve()
		if err != nil || got != "plain" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("环境变量", func(t *testing.T) {
		t.Setenv("JIAOAG_TEST_SECRET", "from-env")
		got, err := Secret("$JIAOAG_TEST_SECRET").Resolve()
		if err != nil || got != "from-env" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("文件", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(p, []byte("  from-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Secret("file:" + p).Resolve()
		if err != nil || got != "from-file" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})

	t.Run("空值", func(t *testing.T) {
		if _, err := Secret("").Resolve(); err == nil {
			t.Fatal("期望报错")
		}
	})

	t.Run("隐藏明文", func(t *testing.T) {
		if got := Secret("super-secret").String(); strings.Contains(got, "super-secret") {
			t.Fatalf("String() 泄漏了明文: %s", got)
		}
	})
}

func TestWriteExampleRefusesOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := WriteExample(p); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if err := WriteExample(p); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("期望拒绝覆盖，得到 %v", err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("样例配置应当能被 Load 解析: %v", err)
	}
}

func TestProviderTimeoutAndRetriesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Providers[0]
	if p.TimeoutSeconds != nil || p.MaxRetries != nil {
		t.Fatalf("未配置时字段应保持 nil：%+v", p)
	}
	if got := p.Timeout(); got != 120*time.Second {
		t.Errorf("Timeout() = %s，期望 120s", got)
	}
	if got := p.Retries(); got != 2 {
		t.Errorf("Retries() = %d，期望 2", got)
	}
}

func TestProviderTimeoutAndRetriesExplicitZero(t *testing.T) {
	// max_retries 显式 0 与「未配置」语义不同：前者表示不重试。
	yaml := strings.Replace(minimalYAML, "api_key: k",
		"api_key: k\n    timeout_seconds: 30\n    max_retries: 0", 1)
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.Providers[0]
	if got := p.Timeout(); got != 30*time.Second {
		t.Errorf("Timeout() = %s，期望 30s", got)
	}
	if got := p.Retries(); got != 0 {
		t.Errorf("Retries() = %d，期望 0（显式不重试）", got)
	}
}

func TestValidateServerRejectsBadProviderLimits(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"超时为零", "timeout_seconds: 0", "timeout_seconds"},
		{"超时为负", "timeout_seconds: -5", "timeout_seconds"},
		{"重试为负", "max_retries: -1", "max_retries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := strings.Replace(minimalYAML, "api_key: k", "api_key: k\n    "+tc.extra, 1)
			cfg, err := Load(writeTemp(t, yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = cfg.ValidateServer()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望 %q 校验错误，得到 %v", tc.want, err)
			}
		})
	}
}

func TestValidateServerAllowsZeroRetries(t *testing.T) {
	yaml := strings.Replace(minimalYAML, "api_key: k", "api_key: k\n    max_retries: 0", 1)
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("max_retries=0 应合法：%v", err)
	}
}

func TestContextBudgetDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ContextBudgetTokens != DefaultContextBudgetTokens {
		t.Fatalf("默认 token 预算 = %d，期望 %d", cfg.Agent.ContextBudgetTokens, DefaultContextBudgetTokens)
	}
	if got := cfg.Agent.ContextBudgetTokenValue(); got != DefaultContextBudgetTokens {
		t.Fatalf("生效预算 = %d，期望 %d", got, DefaultContextBudgetTokens)
	}

	// 旧写法仍然有效，按 2 字符/token 折算。
	legacy := strings.Replace(minimalYAML, "providers:", "agent:\n  context_budget_chars: 40000\nproviders:", 1)
	cfg, err = Load(writeTemp(t, legacy))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.ContextBudgetTokenValue(); got != 20000 {
		t.Fatalf("chars 折算预算 = %d，期望 20000", got)
	}

	// 两个都配时以 token 为准。
	both := strings.Replace(minimalYAML, "providers:", "agent:\n  context_budget_chars: 40000\n  context_budget_tokens: 5000\nproviders:", 1)
	cfg, err = Load(writeTemp(t, both))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.ContextBudgetTokenValue(); got != 5000 {
		t.Fatalf("双配置时预算 = %d，期望 5000", got)
	}

	tooSmall := strings.Replace(minimalYAML, "providers:", "agent:\n  context_budget_tokens: 500\nproviders:", 1)
	cfg, err = Load(writeTemp(t, tooSmall))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "context_budget_tokens") {
		t.Fatalf("过小的 token 预算应校验失败，得到 %v", err)
	}

	legacySmall := strings.Replace(minimalYAML, "providers:", "agent:\n  context_budget_chars: 500\nproviders:", 1)
	cfg, err = Load(writeTemp(t, legacySmall))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "context_budget_chars") {
		t.Fatalf("过小的字符预算应校验失败，得到 %v", err)
	}
}

func TestContextCompactDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ContextCompact != ContextCompactAuto {
		t.Fatalf("默认压缩模式 = %q，期望 %q", cfg.Agent.ContextCompact, ContextCompactAuto)
	}

	off := strings.Replace(minimalYAML, "providers:", "agent:\n  context_compact: off\nproviders:", 1)
	cfg, err = Load(writeTemp(t, off))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("context_compact=off 应合法：%v", err)
	}

	bad := strings.Replace(minimalYAML, "providers:", "agent:\n  context_compact: always\nproviders:", 1)
	cfg, err = Load(writeTemp(t, bad))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "context_compact") {
		t.Fatalf("非法压缩模式应校验失败，得到 %v", err)
	}
}

func TestResumeQueuedDefaultsAndOverride(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Agent.ResumeQueuedEnabled() {
		t.Fatal("resume_queued 默认应开启")
	}

	off := strings.Replace(minimalYAML, "providers:", "agent:\n  resume_queued: false\nproviders:", 1)
	cfg, err = Load(writeTemp(t, off))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ResumeQueuedEnabled() {
		t.Fatal("resume_queued: false 应关闭续跑")
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("resume_queued: false 应合法：%v", err)
	}
}

func TestApprovalTTLDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ApprovalTTL != DefaultApprovalTTL {
		t.Fatalf("approval_ttl 默认应为 %q，实际 %q", DefaultApprovalTTL, cfg.Agent.ApprovalTTL)
	}
	if got := cfg.Agent.ApprovalTTLDuration(); got != 12*time.Hour {
		t.Fatalf("默认时长应为 12h，实际 %v", got)
	}

	custom := strings.Replace(minimalYAML, "providers:", "agent:\n  approval_ttl: 30m\nproviders:", 1)
	cfg, err = Load(writeTemp(t, custom))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("approval_ttl: 30m 应合法：%v", err)
	}
	if got := cfg.Agent.ApprovalTTLDuration(); got != 30*time.Minute {
		t.Fatalf("时长应为 30m，实际 %v", got)
	}

	// 授权必须有过期时间：0 或负数不能当作「永不过期」放过去。
	for _, bad := range []string{"0", "-1h", "forever"} {
		src := strings.Replace(minimalYAML, "providers:",
			"agent:\n  approval_ttl: "+bad+"\nproviders:", 1)
		cfg, err = Load(writeTemp(t, src))
		if err != nil {
			t.Fatalf("Load(%q): %v", bad, err)
		}
		if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "approval_ttl") {
			t.Fatalf("approval_ttl: %q 应校验失败，得到 %v", bad, err)
		}
	}
}

func TestMemoryDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Memory != MemoryAuto || !cfg.Agent.MemoryEnabled() {
		t.Fatalf("memory 默认应为 %q，实际 %q", MemoryAuto, cfg.Agent.Memory)
	}

	off := strings.Replace(minimalYAML, "providers:", "agent:\n  memory: off\nproviders:", 1)
	cfg, err = Load(writeTemp(t, off))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.MemoryEnabled() {
		t.Fatal("memory: off 应关闭记忆")
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("memory: off 应合法：%v", err)
	}

	bad := strings.Replace(minimalYAML, "providers:", "agent:\n  memory: always\nproviders:", 1)
	cfg, err = Load(writeTemp(t, bad))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("非法记忆模式应校验失败，得到 %v", err)
	}
}

func TestProviderModelsParsed(t *testing.T) {
	yaml := strings.Replace(minimalYAML, "    api_key: k",
		"    api_key: k\n    models:\n      - m-fast\n      - m-pro", 1)
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
	got := cfg.Providers[0].Models
	if len(got) != 2 || got[0] != "m-fast" || got[1] != "m-pro" {
		t.Fatalf("models = %v，期望 [m-fast m-pro]", got)
	}
}

func TestMCPTrustDefaultsToAllow(t *testing.T) {
	yaml := minimalYAML + `mcp:
  servers:
    - name: fs
      command: /bin/true
    - name: guarded
      command: /bin/true
      trust: ask
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
	if got := cfg.MCP.Servers[0].Trust; got != "allow" {
		t.Fatalf("默认 trust = %q，期望 allow", got)
	}
	if got := cfg.MCP.Servers[1].Trust; got != "ask" {
		t.Fatalf("显式 trust = %q，期望 ask", got)
	}

	bad := minimalYAML + `mcp:
  servers:
    - name: fs
      command: /bin/true
      trust: always
`
	cfg, err = Load(writeTemp(t, bad))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "trust") {
		t.Fatalf("非法 trust 应报错，得到 %v", err)
	}
}

func TestSkillsDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.Skills; got != "auto" {
		t.Fatalf("默认 skills = %q，期望 auto", got)
	}
	if dirs := cfg.Agent.SkillDirsOrDefault(); len(dirs) != 1 || dirs[0] != DefaultSkillsDir {
		t.Fatalf("默认技能目录 = %v", dirs)
	}

	home := t.TempDir()
	setHome(t, home)
	yaml := strings.Replace(minimalYAML, "providers:", `agent:
  skills: off
  skills_dirs:
    - ~/my-skills
    - .jiaoag/skills
providers:`, 1)
	cfg, err = Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
	if cfg.Agent.Skills != "off" {
		t.Fatalf("skills = %q", cfg.Agent.Skills)
	}
	if got := cfg.Agent.SkillsDirs[0]; got != filepath.Join(home, "my-skills") {
		t.Fatalf("~ 应展开，得到 %q", got)
	}
	if got := cfg.Agent.SkillsDirs[1]; got != ".jiaoag/skills" {
		t.Fatalf("相对目录应保持原样，得到 %q", got)
	}

	bad := strings.Replace(minimalYAML, "providers:", "agent:\n  skills: always\nproviders:", 1)
	cfg, err = Load(writeTemp(t, bad))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "agent.skills") {
		t.Fatalf("非法 skills 应报错，得到 %v", err)
	}
}

func TestProjectModeDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Project.Mode != ProjectAsk {
		t.Fatalf("project.mode 默认应为 %q，实际 %q", ProjectAsk, cfg.Project.Mode)
	}

	for _, mode := range []string{ProjectAsk, ProjectAuto, ProjectOff} {
		yaml := strings.Replace(minimalYAML, "providers:", "project:\n  mode: "+mode+"\nproviders:", 1)
		cfg, err := Load(writeTemp(t, yaml))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Project.Mode != mode {
			t.Fatalf("project.mode = %q，期望 %q", cfg.Project.Mode, mode)
		}
		if err := cfg.ValidateServer(); err != nil {
			t.Fatalf("mode %q 应合法：%v", mode, err)
		}
	}

	bad := strings.Replace(minimalYAML, "providers:", "project:\n  mode: always\nproviders:", 1)
	cfg, err = Load(writeTemp(t, bad))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "project.mode") {
		t.Fatalf("非法项目模式应校验失败，得到 %v", err)
	}
}

func TestMCPServerToolFilter(t *testing.T) {
	yaml := minimalYAML + `mcp:
  servers:
    - name: lsp
      command: /bin/true
      tools:
        include: [definition, " references "]
        exclude: [edit_file, rename_symbol]
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
	include, exclude := cfg.MCP.Servers[0].ToolFilter()
	if !reflect.DeepEqual(include, []string{"definition", "references"}) {
		t.Fatalf("include = %v，期望去掉空白后的两项", include)
	}
	if !reflect.DeepEqual(exclude, []string{"edit_file", "rename_symbol"}) {
		t.Fatalf("exclude = %v", exclude)
	}

	bad := minimalYAML + `mcp:
  servers:
    - name: lsp
      command: /bin/true
      tools:
        include: [""]
`
	cfg, err = Load(writeTemp(t, bad))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "tools.include") {
		t.Fatalf("空工具名应报错，得到 %v", err)
	}
}

func TestPolicyFeedValidation(t *testing.T) {
	pub := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))

	load := func(extra string) *Config {
		t.Helper()
		cfg, err := Load(writeTemp(t, strings.Replace(minimalYAML, "providers:", "policy:\n"+extra+"providers:", 1)))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg
	}

	// 都不配：不用 feed，正常。
	if cfg := load("  dir: /tmp/p\n"); func() error { return cfg.ValidateServer() }() != nil {
		t.Fatalf("未配置 feed 时不应报错: %v", cfg.ValidateServer())
	}

	// url 与 keys 都配：通过，且能解析出公钥。
	both := load("  dir: /tmp/p\n  feed:\n    url: https://example.com/feed.yaml\n    keys: [" + pub + "]\n")
	if err := both.ValidateServer(); err != nil {
		t.Fatalf("完整配置应通过: %v", err)
	}
	keys, err := both.FeedKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("FeedKeys = %v, %v", keys, err)
	}

	// 只配一半都算错：会让「以为有签名保护」静默落空。
	if cfg := load("  feed:\n    url: https://example.com/feed.yaml\n"); cfg.ValidateServer() == nil ||
		!strings.Contains(cfg.ValidateServer().Error(), "policy.feed.keys") {
		t.Fatalf("只配 url 应报错")
	}
	if cfg := load("  feed:\n    keys: [" + pub + "]\n"); cfg.ValidateServer() == nil ||
		!strings.Contains(cfg.ValidateServer().Error(), "policy.feed.url") {
		t.Fatalf("只配公钥应报错")
	}
	if cfg := load("  feed:\n    url: https://example.com/feed.yaml\n    keys: [not-base64]\n"); cfg.ValidateServer() == nil ||
		!strings.Contains(cfg.ValidateServer().Error(), "policy.feed.keys") {
		t.Fatalf("非法公钥应报错")
	}
}
