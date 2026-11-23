package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

func TestMCPCommandAsksForList(t *testing.T) {
	m := newModel(t)
	m.input = "/mcp"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/mcp 应发出 mcp.list 请求")
	}
	if m.input != "" {
		t.Fatalf("命令输入应被清空：%q", m.input)
	}
	if m.mcpPanel != nil {
		t.Fatal("结果返回前不应已有面板")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "正在获取 MCP server 列表") {
		t.Fatalf("缺少加载提示：%v", m.dialog)
	}
}

func TestMCPListResultOpensPanel(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeMCPListResult, protocol.MCPListResultPayload{
		Servers: []protocol.MCPServerInfo{
			{
				Name:      "fs",
				Command:   "npx -y server-filesystem /tmp/ws",
				Trust:     true,
				Source:    "config",
				Connected: true,
				Version:   "1.2.0",
				Tools:     14,
				TimeoutMS: 60000,
			},
			{Name: "lsp", Command: "mcp-language-server -lsp gopls", Source: "project"},
		},
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}

	if cmd := m.handleEnvelope(env); cmd != nil {
		t.Fatal("打开面板不该触发额外命令")
	}
	if m.mcpPanel == nil {
		t.Fatal("应打开 MCP 面板")
	}
	view := m.renderMCPPanel()
	for _, want := range []string{"2 台", "14 个工具", "fs", "14 工具", "配置即信任", "v1.2.0", "npx -y server-filesystem", "lsp", "未连接", "逐次审批", "project"} {
		if !strings.Contains(view, want) {
			t.Fatalf("面板缺少 %q：\n%s", want, view)
		}
	}
}

func TestMCPPanelEmptyListDoesNotOpen(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeMCPListResult, protocol.MCPListResultPayload{})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}

	m.handleEnvelope(env)
	if m.mcpPanel != nil {
		t.Fatal("空列表不该开一个没有任何内容的面板")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "没有配置 MCP server") {
		t.Fatalf("应提示没配置：%v", m.dialog)
	}
}

func TestMCPPanelKeysScrollAndClose(t *testing.T) {
	m := newModel(t)
	servers := make([]protocol.MCPServerInfo, 8)
	for i := range servers {
		servers[i] = protocol.MCPServerInfo{Name: string(rune('a' + i)), Command: "cmd", Source: "config"}
	}
	m.mcpPanel = newMCPPanel(servers)

	// 面板打开时按键只归面板，不能漏进输入行。
	m.input = ""
	m.Update(tea.KeyPressMsg{Code: 'j'})
	if m.mcpPanel.selected != 1 {
		t.Fatalf("j 应下移一格，得到 %d", m.mcpPanel.selected)
	}
	if m.input != "" {
		t.Fatalf("j 不该进输入行：%q", m.input)
	}
	for i := 0; i < 7; i++ {
		m.Update(tea.KeyPressMsg{Code: 'j'})
	}
	if m.mcpPanel.selected != 7 {
		t.Fatalf("移动应停在最后一项，得到 %d", m.mcpPanel.selected)
	}
	// 滚动窗口要跟着走，否则最后几台永远看不到。
	if m.mcpPanel.offset != 7-mcpPanelHeight+1 {
		t.Fatalf("offset = %d，期望 %d", m.mcpPanel.offset, 7-mcpPanelHeight+1)
	}
	if view := m.renderMCPPanel(); !strings.Contains(view, "▶ h") {
		t.Fatalf("选中项应可见：\n%s", view)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mcpPanel != nil {
		t.Fatal("esc 应关闭面板")
	}
}

func TestMCPPanelClearedOnReset(t *testing.T) {
	m := newModel(t)
	m.mcpPanel = newMCPPanel([]protocol.MCPServerInfo{{Name: "fs"}})
	m.resetView()
	if m.mcpPanel != nil {
		t.Fatal("全量重放前应清掉面板")
	}
}
