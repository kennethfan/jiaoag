package main

import (
	"testing"

	"github.com/kennethfan/jiaoag/internal/config"
)

func TestMcpServersTrustMapping(t *testing.T) {
	disabled := false
	cfg := &config.Config{MCP: config.MCP{Servers: []config.MCPServer{
		{Name: "trusted", Command: "/bin/true", Trust: "allow"},
		{Name: "guarded", Command: "/bin/true", Trust: "ask"},
		{Name: "off", Command: "/bin/true", Trust: "allow", Enabled: &disabled},
	}}}
	out := mcpServers(cfg)
	if len(out) != 2 {
		t.Fatalf("禁用项应跳过，得到 %d 台", len(out))
	}
	if !out[0].Trust {
		t.Fatal("trust: allow 应映射为 Trusted")
	}
	if out[1].Trust {
		t.Fatal("trust: ask 不应映射为 Trusted")
	}
}
