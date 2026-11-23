package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// mcpPanelView 是 /mcp 打开的只读面板：看得到有哪些外部 server、各几个工具。
//
// 只读是有意的：改 trust 等于改安全边界，那件事应该在配置里做、由人看 diff，
// 不该是面板上按一下。这里只回答「现在跑的是什么」。
type mcpPanelView struct {
	servers  []protocol.MCPServerInfo
	selected int
	offset   int
}

// mcpPanelHeight 是一屏能放几台 server。每台占两行（标题 + 命令），
// 所以实际高度是这个数字的两倍。
const mcpPanelHeight = 5

func newMCPPanel(servers []protocol.MCPServerInfo) *mcpPanelView {
	return &mcpPanelView{servers: servers}
}

func (m *Model) handleMCPKey(key string) (tea.Model, tea.Cmd) {
	v := m.mcpPanel
	switch key {
	case "esc", "enter":
		m.mcpPanel = nil
		return m, nil
	case "up", "k":
		if v.selected > 0 {
			v.selected--
		}
	case "down", "j":
		if v.selected < len(v.servers)-1 {
			v.selected++
		}
	}
	if v.selected < v.offset {
		v.offset = v.selected
	}
	if v.selected >= v.offset+mcpPanelHeight {
		v.offset = v.selected - mcpPanelHeight + 1
	}
	return m, nil
}

func (m *Model) renderMCPPanel() string {
	v := m.mcpPanel
	var b strings.Builder
	total := 0
	for _, srv := range v.servers {
		total += srv.Tools
	}
	fmt.Fprintf(&b, "⇄ MCP server（%d 台，%d 个工具）\n", len(v.servers), total)
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	end := v.offset + mcpPanelHeight
	if end > len(v.servers) {
		end = len(v.servers)
	}
	for i := v.offset; i < end; i++ {
		b.WriteString(strings.Join(wrapText(mcpServerLine(v.servers[i], i == v.selected), panelWidth), "\n"))
		b.WriteByte('\n')
	}
	b.WriteString("[↑↓/jk] 选择  [esc] 关闭")
	return b.String()
}

// mcpServerLine 把一台 server 渲染成两行：状态行 + 启动命令。
func mcpServerLine(srv protocol.MCPServerInfo, selected bool) string {
	marker := "  "
	if selected {
		marker = "▶ "
	}
	state := "未连接"
	if srv.Connected {
		state = fmt.Sprintf("%d 工具", srv.Tools)
	}
	trust := "逐次审批"
	if srv.Trust {
		trust = "配置即信任"
	}
	version := srv.Version
	if version == "" {
		version = "未知版本"
	}
	head := fmt.Sprintf("%s%s  [%s]  [%s]  [%s]  v%s", marker, srv.Name, state, trust, srv.Source, version)
	return head + "\n      " + srv.Command
}
