package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// modelPanelView 是 /models 打开的模型选择面板。
type modelPanelView struct {
	models   []string
	selected int
	source   string
	offset   int
	// forTask 为 true 时，选中只作用于下一条任务（/model）；否则改会话（/models）。
	forTask bool
}

const modelPanelHeight = 12

func newModelPanel(models []string, current, source string) *modelPanelView {
	return newModelPanelFor(models, current, source, false)
}

// newModelPanelFor 按用途构造面板：会话级或仅下一条任务。
func newModelPanelFor(models []string, current, source string, forTask bool) *modelPanelView {
	v := &modelPanelView{models: models, source: source, forTask: forTask}
	for i, m := range models {
		if m == current {
			v.selected = i
			break
		}
	}
	return v
}

// noteProject 在会话创建时提示项目级扩展的状态。
//
// 未确认时刻意说得具体一点：用户需要知道「为什么 AGENTS.md 没生效」，
// 否则只会以为功能坏了。
func (m *Model) noteProject(info *protocol.ProjectInfo) {
	if info == nil || !info.Detected {
		return
	}
	if info.Loaded {
		m.appendLine("⋯ 项目级扩展已生效：" + info.Summary)
		return
	}
	if info.Mode == "off" {
		return
	}
	m.appendLine("⚠ 检测到项目级扩展（" + info.Summary + "），未确认已跳过；输入 /trust 查看并启用")
}

// modelLabel 用于状态行：供应商为空时只显示模型名。
func modelLabel(provider, model string) string {
	if provider == "" {
		return model
	}
	return provider + "/" + model
}

func (m *Model) handleModelKey(key string) (tea.Model, tea.Cmd) {
	v := m.modelPanel
	switch key {
	case "esc":
		m.modelPanel = nil
		return m, nil
	case "up", "k":
		if v.selected > 0 {
			v.selected--
		}
	case "down", "j":
		if v.selected < len(v.models)-1 {
			v.selected++
		}
	case "enter":
		chosen := v.models[v.selected]
		m.modelPanel = nil
		if v.forTask {
			// 只覆盖这一条任务：不动会话，提交后自动恢复。
			m.armTaskModel(chosen)
			return m, nil
		}
		m.appendLine("正在切换模型：" + chosen)
		return m, sendCmd(m.conn, protocol.TypeSessionUpdate, protocol.SessionUpdatePayload{
			SessionID: m.sessionID,
			Provider:  m.sessionProvider,
			Model:     chosen,
		})
	}
	if v.selected < v.offset {
		v.offset = v.selected
	}
	if v.selected >= v.offset+modelPanelHeight {
		v.offset = v.selected - modelPanelHeight + 1
	}
	return m, nil
}

func (m *Model) renderModelPanel() string {
	v := m.modelPanel
	var b strings.Builder
	source := "配置"
	if v.source == "api" {
		source = "供应商接口"
	}
	purpose := "切换会话模型"
	if v.forTask {
		purpose = "只用于下一条任务"
	}
	fmt.Fprintf(&b, "⇄ 选择模型（%d 个，来源：%s，%s）\n", len(v.models), source, purpose)
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	end := v.offset + modelPanelHeight
	if end > len(v.models) {
		end = len(v.models)
	}
	for i := v.offset; i < end; i++ {
		marker := "  "
		if i == v.selected {
			marker = "▶ "
		}
		line := marker + v.models[i]
		switch {
		case v.forTask && m.forcedModel != "" && v.models[i] == m.forcedModel:
			line += "  (下一条)"
		case v.models[i] == m.sessionModel:
			line += "  (当前)"
		}
		b.WriteString(strings.Join(wrapText(line, panelWidth), "\n"))
		b.WriteByte('\n')
	}
	b.WriteString("[↑↓/jk] 选择  [enter] 应用  [esc] 关闭")
	return b.String()
}
