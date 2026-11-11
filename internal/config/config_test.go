package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.MaxRounds != 24 {
		t.Errorf("MaxRounds = %d，期望 24", cfg.Agent.MaxRounds)
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
	if cfg.Agent.ContextBudgetChars != 60000 {
		t.Fatalf("默认上下文预算 = %d，期望 60000", cfg.Agent.ContextBudgetChars)
	}

	tooSmall := strings.Replace(minimalYAML, "providers:", "agent:\n  context_budget_chars: 500\nproviders:", 1)
	cfg, err = Load(writeTemp(t, tooSmall))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ValidateServer(); err == nil || !strings.Contains(err.Error(), "context_budget_chars") {
		t.Fatalf("过小的预算应校验失败，得到 %v", err)
	}
}
