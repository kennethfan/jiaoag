package boot

import (
	"testing"

	"github.com/kennethfan/jiaoag/internal/config"
)

// mcpServers 把配置翻译成 MCP 管理器要的形态：禁用项跳过、trust 只在
// 显式 allow 时为真。这条翻译错了会让「ask」变成免审，所以单独锁住。
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
