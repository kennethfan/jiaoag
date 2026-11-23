package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// 输入 /models 后应发出 provider.models 请求，而不是当成普通提问提交。
func TestModelsCommandAsksForModels(t *testing.T) {
	m := newModel(t)
	m.input = "/models"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/models 应发出模型列表请求")
	}
	if m.input != "" {
		t.Fatalf("命令输入应被清空：%q", m.input)
	}
	if m.modelPanel != nil {
		t.Fatal("结果返回前不应已有面板")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "正在获取模型列表") {
		t.Fatalf("缺少加载提示：%v", m.dialog)
	}
}

// 服务端返回列表后开面板，当前模型默认选中。
func TestModelsResultOpensPanelAtCurrentModel(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-2.5-pro"

	env, err := protocol.New(protocol.TypeProviderModelsResult, protocol.ProviderModelsResultPayload{
		Models: []string{"agnes-3.0-flash", "agnes-2.5-pro", "agnes-2.5-flash"},
		Source: "config",
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	if cmd := m.handleEnvelope(env); cmd != nil {
		t.Fatal("打开面板不应产生额外命令")
	}
	if m.modelPanel == nil {
		t.Fatal("应打开模型面板")
	}
	if got := m.modelPanel.models[m.modelPanel.selected]; got != "agnes-2.5-pro" {
		t.Fatalf("默认选中 %q，期望当前模型", got)
	}
	view := m.renderModelPanel()
	if !strings.Contains(view, "来源：配置") || !strings.Contains(view, "(当前)") {
		t.Fatalf("面板缺少来源或当前标记：\n%s", view)
	}
}

// 选中另一个模型后回车：关闭面板并发出 session.update。
func TestModelPanelEnterSendsSessionUpdate(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "agnes-3.0-flash"
	m.modelPanel = newModelPanel([]string{"agnes-3.0-flash", "agnes-2.5-pro"}, m.sessionModel, "config")

	if _, cmd := m.handleModelKey("down"); cmd != nil {
		t.Fatal("移动不应发请求")
	}
	if m.modelPanel.selected != 1 {
		t.Fatalf("向下移动后选中 %d", m.modelPanel.selected)
	}
	_, cmd := m.handleModelKey("enter")
	if cmd == nil {
		t.Fatal("回车应发出 session.update")
	}
	if m.modelPanel != nil {
		t.Fatal("应用后应关闭面板")
	}
	if got := strings.Join(m.dialog, "\n"); !strings.Contains(got, "agnes-2.5-pro") {
		t.Fatalf("缺少切换提示：%v", m.dialog)
	}
}

func TestModelPanelEscCloses(t *testing.T) {
	m := newModel(t)
	m.modelPanel = newModelPanel([]string{"a", "b"}, "a", "api")

	if _, cmd := m.handleModelKey("esc"); cmd != nil {
		t.Fatal("关闭面板不应发请求")
	}
	if m.modelPanel != nil {
		t.Fatal("esc 应关闭面板")
	}
}

// 事件回放也要能更新当前模型：重连后状态行不能停在旧模型。
func TestSessionUpdatedEventRefreshesModel(t *testing.T) {
	m := newModel(t)
	m.sessionProvider = "agnes"
	m.sessionModel = "old"

	raw, err := json.Marshal(domain.SessionUpdatedPayload{Provider: "agnes", Model: "new"})
	if err != nil {
		t.Fatalf("序列化载荷: %v", err)
	}
	env, err := protocol.New(protocol.TypeEvent, protocol.StreamEventPayload{
		Kind:    string(domain.EventSessionUpdated),
		Payload: raw,
	})
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	m.handleEvent(env)
	if m.sessionModel != "new" {
		t.Fatalf("模型未更新：%q", m.sessionModel)
	}
}
