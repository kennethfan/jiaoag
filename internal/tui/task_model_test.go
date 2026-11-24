package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

func modelResult(t *testing.T, m *Model, models ...string) {
	t.Helper()
	env, err := protocol.New(protocol.TypeProviderModelsResult, protocol.ProviderModelsResultPayload{
		Models: models,
		Source: "config",
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	m.handleEnvelope(env)
}

// /model 与 /models 的区别在于用途：前者只作用于下一条任务。
func TestModelCommandAsksForTaskModel(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-2.5-pro"
	m.input = "/model"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/model 应发出模型列表请求")
	}
	if !m.modelsForTask {
		t.Fatal("应标记列表用于下一条任务")
	}
}

func TestModelCommandWithNameArmsDirectly(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-2.5-pro"
	m.input = "/model agnes-3.0-flash"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("带参数时不应再请求列表")
	}
	if m.forcedModel != "agnes-3.0-flash" {
		t.Fatalf("未绑定模型：%q", m.forcedModel)
	}
	if m.sessionModel != "agnes-2.5-pro" {
		t.Fatalf("会话模型被改动了：%q", m.sessionModel)
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "下一条任务使用模型") {
		t.Fatalf("缺少提示：%v", m.dialog)
	}
}

func TestModelPanelForTaskArmsInsteadOfSwitchingSession(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-2.5-pro"
	m.modelsForTask = true
	modelResult(t, m, "agnes-2.5-pro", "agnes-3.0-flash")
	if m.modelPanel == nil {
		t.Fatal("应打开面板")
	}
	m.modelPanel.selected = 1

	_, cmd := m.handleModelKey("enter")
	if cmd != nil {
		t.Fatal("单次模型不应发出 session.update")
	}
	if m.forcedModel != "agnes-3.0-flash" {
		t.Fatalf("未绑定模型：%q", m.forcedModel)
	}
}

func TestTaskSubmitPayloadCarriesForcedModelThenClears(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-2.5-pro"
	m.forcedModel = "agnes-3.0-flash"

	p := m.taskSubmitPayload("干活")
	if p.Provider != "agnes" || p.Model != "agnes-3.0-flash" {
		t.Fatalf("载荷未带单次模型：%+v", p)
	}

	m.input = "干活"
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("应发出 task.submit")
	}
	if m.forcedModel != "" {
		t.Fatalf("提交后应清空单次模型：%q", m.forcedModel)
	}
}

func TestModelPanelForTaskShowsPurposeAndArmedMark(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-2.5-pro"
	m.forcedModel = "agnes-3.0-flash"
	m.modelsForTask = true
	modelResult(t, m, "agnes-2.5-pro", "agnes-3.0-flash")

	view := m.renderModelPanel()
	if !strings.Contains(view, "只用于下一条任务") {
		t.Fatalf("面板缺少用途说明：\n%s", view)
	}
	if !strings.Contains(view, "(下一条)") {
		t.Fatalf("面板缺少已绑定标记：\n%s", view)
	}
}
