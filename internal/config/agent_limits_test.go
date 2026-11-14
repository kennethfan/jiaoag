package config

import (
	"strings"
	"testing"
)

func TestAgentLimitsValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"并发数为负", func(c *Config) { c.Agent.MaxConcurrentTasks = -1 }, "max_concurrent_tasks"},
		{"队列深度为负", func(c *Config) { c.Agent.MaxQueueDepth = -1 }, "max_queue_depth"},
		{"RPM 为负", func(c *Config) { c.Providers = append(c.Providers, Provider{Name: "p", RPM: -1}) }, "rpm"},
		{"TPM 为负", func(c *Config) { c.Providers = append(c.Providers, Provider{Name: "p", TPM: -1}) }, "tpm"},
	}
	for _, c := range cases {
		cfg := validConfig()
		c.edit(cfg)
		err := cfg.ValidateClient()
		if err == nil {
			t.Fatalf("%s：应当报错", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：错误信息应提到 %q，得到 %v", c.name, c.want, err)
		}
	}
}
