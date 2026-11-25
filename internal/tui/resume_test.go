package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// sessionListFrame 构造 session.list.result 帧，sessions 的顺序即服务端给出的
// 顺序（按 updated_at 倒序）。
func sessionListFrame(t *testing.T, sessions ...protocol.SessionPayload) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(protocol.TypeSessionListResult, protocol.SessionListResultPayload{Sessions: sessions})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	return env
}

func TestContinueLastPicksNewestSessionInWorkspace(t *testing.T) {
	m := newModel(t)
	m.opts.ContinueLast = true
	m.sessionID = ""
	m.resuming = true

	env := sessionListFrame(t,
		protocol.SessionPayload{ID: "ses_other", Workspace: t.TempDir(), Provider: "agnes", Model: "m-other"},
		protocol.SessionPayload{ID: "ses_want", Workspace: m.opts.Workspace, Provider: "agnes", Model: "agnes-3.0-flash", Title: "修边界"},
	)
	cmd := m.handleEnvelope(env)
	if cmd == nil {
		t.Fatal("应该发出 event.subscribe")
	}
	if m.sessionID != "ses_want" {
		t.Fatalf("应继续本目录的会话，实际 %q", m.sessionID)
	}
	if m.sessionModel != "agnes-3.0-flash" || m.sessionProvider != "agnes" {
		t.Fatalf("模型信息没跟上：%s/%s", m.sessionProvider, m.sessionModel)
	}
	if m.resuming {
		t.Fatal("拿到列表后应清掉 resuming")
	}
	joined := strings.Join(m.dialog, "\n")
	// 表头只带标题，不摆会话 id。
	if !strings.Contains(joined, "继续会话：修边界") {
		t.Fatalf("缺少带标题的继续提示：%s", joined)
	}
	if strings.Contains(joined, "ses_want") {
		t.Fatalf("表头不该出现会话 id：%s", joined)
	}
}

func TestContinueLastFallsBackToNewSession(t *testing.T) {
	m := newModel(t)
	m.opts.ContinueLast = true
	m.sessionID = ""
	m.resuming = true

	env := sessionListFrame(t,
		protocol.SessionPayload{ID: "ses_other", Workspace: t.TempDir()},
	)
	cmd := m.handleEnvelope(env)
	if cmd == nil {
		t.Fatal("没有可继续的会话时应发出 session.create")
	}
	if m.sessionID != "" {
		t.Fatalf("不应该选中别的目录的会话，实际 %q", m.sessionID)
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "已新建") {
		t.Fatalf("应提示已新建：%v", m.dialog)
	}
}

func TestSessionListIgnoredWithoutContinueFlag(t *testing.T) {
	m := newModel(t)
	m.sessionID = "ses_keep"
	if cmd := m.handleEnvelope(sessionListFrame(t, protocol.SessionPayload{ID: "ses_x", Workspace: m.opts.Workspace})); cmd != nil {
		t.Fatal("没在等列表回包时不应该有动作")
	}
	if m.sessionID != "ses_keep" {
		t.Fatalf("会话被意外改写：%q", m.sessionID)
	}
}

// -c 首次连接必须先问会话列表，而不是直接新建。
func TestContinueLastAsksSessionListOnConnect(t *testing.T) {
	m := newModel(t)
	m.opts.ContinueLast = true
	m.sessionID = "" // newModel 默认带一个会话 id，首次连接要模拟「没有」

	_, cmd := m.Update(connectedMsg{conn: nil})
	if cmd == nil {
		t.Fatal("应该发出请求帧")
	}
	if !m.resuming {
		t.Fatal("应进入等待列表状态")
	}
	if !strings.Contains(m.status, "正在查找") {
		t.Fatalf("状态行没提示：%q", m.status)
	}
}

// 列表回包的载荷必须能被解出来：这条守的是 payload 结构，不是业务逻辑。
func TestSessionListPayloadRoundTrip(t *testing.T) {
	env := sessionListFrame(t, protocol.SessionPayload{ID: "ses_a", Workspace: "/tmp/a"})
	var p protocol.SessionListResultPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("解载荷: %v", err)
	}
	if len(p.Sessions) != 1 || p.Sessions[0].ID != "ses_a" {
		t.Fatalf("载荷不对：%+v", p)
	}
}
