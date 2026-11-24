package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withProvider 补一个最小可用的 provider，让 ValidateServer 能走到令牌校验。
func withProvider(c *Config) *Config {
	c.Providers = []Provider{{
		Name:    "p",
		Type:    "openai",
		BaseURL: "http://127.0.0.1:1/v1",
		Model:   "m",
		APIKey:  "k",
	}}
	return c
}

func TestServerTokensParsed(t *testing.T) {
	dir := t.TempDir()
	roFile := filepath.Join(dir, "ro.token")
	if err := os.WriteFile(roFile, []byte("ro-secret\n"), 0o600); err != nil {
		t.Fatalf("写入只读令牌文件: %v", err)
	}
	cfg, err := Load(writeTemp(t, `
server:
  listen: 127.0.0.1:8787
  tokens:
    - name: ops
      value: ops-secret
    - name: viewer
      value: file:`+roFile+`
      read_only: true
storage:
  db_path: /tmp/x.db
  output_dir: /tmp/out
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := withProvider(cfg).ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}

	all := cfg.Server.AllTokens()
	if len(all) != 2 || all[0].Name != "ops" || all[1].Name != "viewer" {
		t.Fatalf("AllTokens = %+v", all)
	}
	if !all[1].ReadOnly {
		t.Fatal("viewer 应该是只读令牌")
	}
	secret, err := cfg.Server.ClientToken()
	if err != nil {
		t.Fatalf("ClientToken: %v", err)
	}
	if got, _ := secret.Resolve(); got != "ops-secret" {
		t.Fatalf("客户端应拿到第一个可写令牌，实际 %q", got)
	}
}

func TestServerTokensLegacyCompatible(t *testing.T) {
	cfg := &Config{
		Server: Server{Listen: "127.0.0.1:8787", Token: "legacy"},
	}
	cfg.applyDefaults()
	if err := withProvider(cfg).ValidateServer(); err != nil {
		t.Fatalf("ValidateServer: %v", err)
	}
	all := cfg.Server.AllTokens()
	if len(all) != 1 || all[0].Name != "default" || all[0].ReadOnly {
		t.Fatalf("AllTokens = %+v", all)
	}
	secret, err := cfg.Server.ClientToken()
	if err != nil {
		t.Fatalf("ClientToken: %v", err)
	}
	if got, _ := secret.Resolve(); got != "legacy" {
		t.Fatalf("ClientToken = %q", got)
	}
}

func TestServerTokensValidation(t *testing.T) {
	base := func(tokens ...ServerToken) *Config {
		c := &Config{Server: Server{Listen: "127.0.0.1:8787", Tokens: tokens}}
		c.applyDefaults()
		return withProvider(c)
	}
	cases := []struct {
		name   string
		cfg    *Config
		expect string
	}{
		{"缺名字", base(ServerToken{Value: "x"}), "name"},
		{"名字重复", base(ServerToken{Name: "a", Value: "1"}, ServerToken{Name: "a", Value: "2"}), "重复"},
		{"名字非法字符", base(ServerToken{Name: "a b", Value: "x"}), "name"},
		{"名字过长", base(ServerToken{Name: strings.Repeat("a", 33), Value: "x"}), "name"},
		{"值为空", base(ServerToken{Name: "a"}), "未配置"},
		{"一个都没配", base(), "至少要配置一个"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateServer()
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.expect)) {
				t.Fatalf("错误 %q 不含 %q", err.Error(), tc.expect)
			}
		})
	}
}

func TestClientRequiresWritableToken(t *testing.T) {
	cfg := &Config{
		Server: Server{Listen: "127.0.0.1:8787", Tokens: []ServerToken{
			{Name: "viewer", Value: "ro", ReadOnly: true},
		}},
	}
	cfg.applyDefaults()
	if err := withProvider(cfg).ValidateServer(); err != nil {
		t.Fatalf("服务端配置本身合法: %v", err)
	}
	err := cfg.ValidateClient()
	if err == nil {
		t.Fatal("只有只读令牌时客户端配置应报错")
	}
	if !strings.Contains(err.Error(), "可写令牌") {
		t.Fatalf("错误信息应说明缺少可写令牌: %v", err)
	}
}
