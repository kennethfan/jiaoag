package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// 输入 /skill 后应发出 skill.list 请求，而不是当成普通提问。
func TestSkillCommandAsksForList(t *testing.T) {
	m := newModel(t)
	m.input = "/skill"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/skill 应发出技能列表请求")
	}
	if m.input != "" {
		t.Fatalf("命令输入应被清空：%q", m.input)
	}
	if m.skillPanel != nil {
		t.Fatal("结果返回前不应已有面板")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "正在获取技能列表") {
		t.Fatalf("缺少加载提示：%v", m.dialog)
	}
}

// 带参数时直接点名，不打开面板。
func TestSkillCommandWithNameArmsDirectly(t *testing.T) {
	m := newModel(t)
	m.input = "/skill grilling"

	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("直接点名不该发请求（校验在服务端做）")
	}
	if m.skillPanel != nil {
		t.Fatal("带参数时不该打开面板")
	}
	if len(m.forcedSkills) != 1 || m.forcedSkills[0] != "grilling" {
		t.Fatalf("应指定 grilling，得到 %v", m.forcedSkills)
	}
}

// 列表返回后开面板，仅手动技能要有标记，选中技能显示说明。
func TestSkillListResultOpensPanel(t *testing.T) {
	m := newModel(t)
	// 面板只应由用户显式 /skill 触发；会话建立时的自动拉取只更新补全缓存。
	m.skillPanelRequested = true
	env, err := protocol.New(protocol.TypeSkillListResult, protocol.SkillListResultPayload{
		Skills: []protocol.SkillInfo{
			{Name: "auto-one", Description: "自动技能"},
			{Name: "grilling", Description: "反复追问", Manual: true},
		},
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	if cmd := m.handleEnvelope(env); cmd != nil {
		t.Fatal("打开面板不应产生额外命令")
	}
	if m.skillPanel == nil {
		t.Fatal("应打开技能面板")
	}
	view := m.renderSkillPanel()
	if !strings.Contains(view, "[仅手动]") {
		t.Fatalf("缺少仅手动标记：\n%s", view)
	}
	if !strings.Contains(view, "说明：自动技能") {
		t.Fatalf("缺少选中技能的说明：\n%s", view)
	}
}

// 回车指定技能：只武装下一条任务，不发任何帧。
func TestSkillPanelEnterArms(t *testing.T) {
	m := newModel(t)
	m.skillPanel = newSkillPanel([]protocol.SkillInfo{
		{Name: "auto-one"},
		{Name: "grilling", Manual: true},
	}, "")

	if _, cmd := m.handleSkillKey("down"); cmd != nil {
		t.Fatal("移动不应发请求")
	}
	_, cmd := m.handleSkillKey("enter")
	if cmd != nil {
		t.Fatal("指定技能是本地状态，不发帧")
	}
	if m.skillPanel != nil {
		t.Fatal("指定后应关闭面板")
	}
	if len(m.forcedSkills) != 1 || m.forcedSkills[0] != "grilling" {
		t.Fatalf("应指定 grilling，得到 %v", m.forcedSkills)
	}
}

// 下一条任务提交时带上技能，提交后清空；task.queued 事件把加载的技能写进对话。
func TestSubmitCarriesSkillsOnce(t *testing.T) {
	m := newModel(t)
	m.sessionID = "ses_1"
	m.forcedSkills = []string{"grilling"}
	m.input = "帮我 grill 这个方案"

	payload := m.taskSubmitPayload("帮我 grill 这个方案")
	if len(payload.Skills) != 1 || payload.Skills[0] != "grilling" {
		t.Fatalf("task.submit 没带技能：%+v", payload)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("应发出 task.submit")
	}
	if len(m.forcedSkills) != 0 {
		t.Fatalf("提交后应清空点名的技能，得到 %v", m.forcedSkills)
	}

	// 回放时也要能看到这次任务加载了哪些技能。
	raw, err := json.Marshal(map[string]any{"prompt": "帮我 grill 这个方案", "skills": []string{"grilling"}})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    "task.queued",
		Payload: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.handleEvent(ev)
	if !strings.Contains(strings.Join(m.dialog, "\n"), "加载技能：grilling") {
		t.Fatalf("task.queued 没写出加载的技能：%v", m.dialog)
	}
}

// 面板里按 x 清除已指定的技能。
func TestSkillPanelClearsArmed(t *testing.T) {
	m := newModel(t)
	m.forcedSkills = []string{"grilling"}
	m.skillPanel = newSkillPanel([]protocol.SkillInfo{{Name: "grilling"}}, "grilling")

	if _, cmd := m.handleSkillKey("x"); cmd != nil {
		t.Fatal("清除不发帧")
	}
	if m.skillPanel != nil || len(m.forcedSkills) != 0 {
		t.Fatalf("应清除并关闭：panel=%v armed=%v", m.skillPanel, m.forcedSkills)
	}
}
