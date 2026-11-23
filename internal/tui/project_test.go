package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// /trust 要发确认请求，而不是被当成普通提问发给模型。
func TestTrustCommandSendsProjectTrust(t *testing.T) {
	m := newModel(t)
	m.sessionID = "ses_1"
	m.input = "/trust"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/trust 应发出 project.trust 请求")
	}
	if m.input != "" {
		t.Fatalf("命令输入应被清空：%q", m.input)
	}
}

func TestUntrustCommandSendsProjectRevoke(t *testing.T) {
	m := newModel(t)
	m.sessionID = "ses_1"
	m.input = "/untrust"

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/untrust 应发出 project.revoke 请求")
	}
}

// 项目扩展被检测到但未确认时，要明确提示怎么启用。
func TestNoteProjectWarnsWhenUntrusted(t *testing.T) {
	m := newModel(t)
	m.noteProject(&protocol.ProjectInfo{
		Mode:     "ask",
		Detected: true,
		Summary:  "提示 1 个、MCP 1 台",
	})
	dialog := strings.Join(m.dialog, "\n")
	if !strings.Contains(dialog, "检测到项目级扩展") || !strings.Contains(dialog, "/trust") {
		t.Fatalf("未确认时应提示 /trust：%v", m.dialog)
	}
}

func TestNoteProjectAnnouncesWhenLoaded(t *testing.T) {
	m := newModel(t)
	m.noteProject(&protocol.ProjectInfo{
		Mode:     "auto",
		Detected: true,
		Loaded:   true,
		Summary:  "提示 2 个",
	})
	if !strings.Contains(strings.Join(m.dialog, "\n"), "项目级扩展已生效：提示 2 个") {
		t.Fatalf("已加载时应提示生效范围：%v", m.dialog)
	}
}

// off 模式与服务端没发现资产时都不该打扰用户。
func TestNoteProjectQuietWhenNothingToDo(t *testing.T) {
	m := newModel(t)
	before := len(m.dialog)

	m.noteProject(nil)
	m.noteProject(&protocol.ProjectInfo{Mode: "off", Detected: true})
	m.noteProject(&protocol.ProjectInfo{Mode: "ask"})

	if len(m.dialog) != before {
		t.Fatalf("没有项目扩展时不该写提示行：%v", m.dialog[before:])
	}
}

// 确认成功的回包要把实际加载了什么说清楚：MCP 的 argv 是最要紧的。
func TestProjectTrustedListsServers(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeProjectTrusted, protocol.ProjectStatusPayload{
		SessionID: "ses_1",
		Project: protocol.ProjectInfo{
			Mode:     "ask",
			Detected: true,
			Trusted:  true,
			Loaded:   true,
			Summary:  "提示 1 个、MCP 1 台",
			Prompts:  []string{"AGENTS.md"},
			Servers:  []string{"repo: npx -y some-server"},
		},
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	if cmd := m.handleEnvelope(env); cmd != nil {
		t.Fatal("确认回包不该再发请求")
	}
	text := strings.Join(m.dialog, "\n")
	for _, want := range []string{"已信任项目级扩展", "提示 1 个、MCP 1 台", "AGENTS.md", "repo: npx -y some-server"} {
		if !strings.Contains(text, want) {
			t.Fatalf("回包提示缺少 %q：\n%s", want, text)
		}
	}
}

func TestProjectTrustedAnnouncesRevoke(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeProjectTrusted, protocol.ProjectStatusPayload{
		SessionID: "ses_1",
		Project:   protocol.ProjectInfo{Mode: "ask", Detected: true},
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	m.handleEnvelope(env)
	if !strings.Contains(strings.Join(m.dialog, "\n"), "不再加载") {
		t.Fatalf("撤销后应说明不再加载：%v", m.dialog)
	}
}
