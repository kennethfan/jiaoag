package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// skillPanelView 是 /skill 打开的技能选择面板。
//
// 与 /models 的区别：选中技能不改会话，只武装「下一条任务」。技能是一次性的
// 上下文成本（正文最多 96 KB），所以不做粘性开关。
type skillPanelView struct {
	skills   []protocol.SkillInfo
	selected int
	offset   int
}

const skillPanelHeight = 12

func newSkillPanel(items []protocol.SkillInfo, armed string) *skillPanelView {
	v := &skillPanelView{skills: items}
	for i, s := range items {
		if s.Name == armed {
			v.selected = i
			break
		}
	}
	return v
}

func (m *Model) handleSkillKey(key string) (tea.Model, tea.Cmd) {
	v := m.skillPanel
	switch key {
	case "esc":
		m.skillPanel = nil
		return m, nil
	case "x":
		m.skillPanel = nil
		m.forcedSkills = nil
		m.appendLine("已清除指定的技能")
		return m, nil
	case "up", "k":
		if v.selected > 0 {
			v.selected--
		}
	case "down", "j":
		if v.selected < len(v.skills)-1 {
			v.selected++
		}
	case "enter":
		chosen := v.skills[v.selected].Name
		m.skillPanel = nil
		m.armSkill(chosen)
		return m, nil
	}
	if v.selected < v.offset {
		v.offset = v.selected
	}
	if v.selected >= v.offset+skillPanelHeight {
		v.offset = v.selected - skillPanelHeight + 1
	}
	return m, nil
}

// armSkill 让下一条任务加载该技能的正文。
//
// 技能名在这里不校验：真正的规则（是否存在、能不能读）在服务端构造任务时
// 判定，客户端自己扫一遍反而会和远端配置不一致。
func (m *Model) armSkill(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	m.forcedSkills = []string{name}
	m.appendLine(fmt.Sprintf("已指定技能 %s：下一条任务会先加载它（[x] 在面板里清除）", name))
}

func (m *Model) renderSkillPanel() string {
	v := m.skillPanel
	var b strings.Builder
	fmt.Fprintf(&b, "⇄ 选择技能（%d 个，选中后下一条任务加载正文）\n", len(v.skills))
	b.WriteString(strings.Repeat("─", panelWidth))
	b.WriteByte('\n')
	end := v.offset + skillPanelHeight
	if end > len(v.skills) {
		end = len(v.skills)
	}
	for i := v.offset; i < end; i++ {
		s := v.skills[i]
		marker := "  "
		if i == v.selected {
			marker = "▶ "
		}
		line := marker + s.Name
		if s.Manual {
			line += "  [仅手动]"
		}
		if len(m.forcedSkills) > 0 && m.forcedSkills[0] == s.Name {
			line += "  [已指定]"
		}
		b.WriteString(strings.Join(wrapText(line, panelWidth), "\n"))
		b.WriteByte('\n')
	}
	if v.selected < len(v.skills) && v.skills[v.selected].Description != "" {
		// 描述只给选中的那一条：65 个技能全展开会把窗口变成滚动条。
		desc := wrapText("说明："+v.skills[v.selected].Description, panelWidth)
		if len(desc) > 2 {
			desc = desc[:2]
		}
		b.WriteString(strings.Join(desc, "\n"))
		b.WriteByte('\n')
	}
	b.WriteString("[↑↓/jk] 选择  [enter] 指定  [x] 清除  [esc] 关闭")
	return b.String()
}
