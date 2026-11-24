package server

import (
	"testing"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// createSessionIn 在指定工作区里建一条会话并返回 id。
func createSessionIn(t *testing.T, c *websocket.Conn, workspace, title string) string {
	t.Helper()
	env, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: workspace,
		Title:     title,
	})
	if err != nil {
		t.Fatalf("构造 session.create: %v", err)
	}
	write(t, c, env)
	created := readEnvelope(t, c)
	if created.Type != protocol.TypeSessionCreated {
		t.Fatalf("建会话回帧 = %s：%s", created.Type, created.Payload)
	}
	var sess protocol.SessionPayload
	if err := created.Decode(&sess); err != nil {
		t.Fatalf("解码 session.created: %v", err)
	}
	return sess.ID
}

// 会话管理走 WS：列表只回同目录，改名落事件，删除后列表变短。
func TestSessionManageOverWS(t *testing.T) {
	ts := newModelsServer(t)
	c := dial(t, wsURL(ts), testToken)
	handshake(t, c)

	ws := t.TempDir()
	first := createSessionIn(t, c, ws, "第一条")
	second := createSessionIn(t, c, ws, "")

	send(t, c, protocol.TypeSessionManage, protocol.SessionManagePayload{SessionID: first})
	manage := readEnvelope(t, c)
	if manage.Type != protocol.TypeSessionManageResult {
		t.Fatalf("期望 session.manage.result，得到 %s：%s", manage.Type, manage.Payload)
	}
	var list protocol.SessionManageResultPayload
	if err := manage.Decode(&list); err != nil {
		t.Fatalf("解码会话列表: %v", err)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("同工作区应有 2 条，得到 %d", len(list.Sessions))
	}
	titles := map[string]string{}
	for _, item := range list.Sessions {
		titles[item.ID] = item.Title
		if item.Workspace != ws {
			t.Fatalf("工作区 = %q，期望 %q", item.Workspace, ws)
		}
	}
	if titles[first] != "第一条" {
		t.Fatalf("标题 = %q", titles[first])
	}

	send(t, c, protocol.TypeSessionRename, protocol.SessionRenamePayload{
		SessionID: second,
		Title:     "改过的标题",
	})
	renamed := readEnvelope(t, c)
	if renamed.Type != protocol.TypeSessionRenamed {
		t.Fatalf("期望 session.renamed，得到 %s：%s", renamed.Type, renamed.Payload)
	}
	var sess protocol.SessionPayload
	if err := renamed.Decode(&sess); err != nil {
		t.Fatalf("解码 session.renamed: %v", err)
	}
	if sess.Title != "改过的标题" {
		t.Fatalf("改名回包标题 = %q", sess.Title)
	}

	send(t, c, protocol.TypeSessionDelete, protocol.SessionDeletePayload{SessionID: second})
	deleted := readEnvelope(t, c)
	if deleted.Type != protocol.TypeSessionDeleted {
		t.Fatalf("期望 session.deleted，得到 %s：%s", deleted.Type, deleted.Payload)
	}

	send(t, c, protocol.TypeSessionManage, protocol.SessionManagePayload{SessionID: first})
	again := readEnvelope(t, c)
	var after protocol.SessionManageResultPayload
	if err := again.Decode(&after); err != nil {
		t.Fatalf("解码会话列表: %v", err)
	}
	if len(after.Sessions) != 1 || after.Sessions[0].ID != first {
		t.Fatalf("删除后应只剩第一条：%+v", after.Sessions)
	}

	// 再删一次：会话不存在，回 bad_request 而不是内部错误。
	send(t, c, protocol.TypeSessionDelete, protocol.SessionDeletePayload{SessionID: second})
	missing := readEnvelope(t, c)
	if missing.Type != protocol.TypeError {
		t.Fatalf("重复删除应回 error，得到 %s", missing.Type)
	}
	var e protocol.ErrorPayload
	if err := missing.Decode(&e); err != nil {
		t.Fatalf("解码 error: %v", err)
	}
	if e.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s", e.Code)
	}

	// 退订之后连接仍然可用：ping 能拿到 pong 就说明会话流被停掉而不是连接被关。
	send(t, c, protocol.TypeEventSubscribe, protocol.EventSubscribePayload{SessionID: first})
	send(t, c, protocol.TypeEventUnsubscribe, protocol.EventUnsubscribePayload{SessionID: first})
	send(t, c, protocol.TypePing, nil)
	pong := readEnvelope(t, c)
	if pong.Type != protocol.TypePong {
		t.Fatalf("退订后连接应仍可用，得到 %s", pong.Type)
	}
}

// 只读令牌能看会话列表，但不能改名或删除。
func TestReadOnlyTokenCannotManageSessions(t *testing.T) {
	cfg := tokenConfig(t,
		config.ServerToken{Name: "ops", Value: "ops-token"},
		config.ServerToken{Name: "viewer", Value: "view-token", ReadOnly: true},
	)
	ts, _ := newTokenServer(t, cfg)

	ops := dial(t, wsURL(ts), "ops-token")
	handshake(t, ops)
	ws := t.TempDir()
	id := createSessionIn(t, ops, ws, "只能看")

	viewer := dial(t, wsURL(ts), "view-token")
	handshake(t, viewer)

	send(t, viewer, protocol.TypeSessionManage, protocol.SessionManagePayload{SessionID: id})
	manage := readEnvelope(t, viewer)
	if manage.Type != protocol.TypeSessionManageResult {
		t.Fatalf("只读令牌应能列会话，得到 %s：%s", manage.Type, manage.Payload)
	}

	send(t, viewer, protocol.TypeSessionRename, protocol.SessionRenamePayload{SessionID: id, Title: "改不动"})
	denied := readEnvelope(t, viewer)
	if denied.Type != protocol.TypeError {
		t.Fatalf("改名应被拒，得到 %s", denied.Type)
	}
	var e protocol.ErrorPayload
	if err := denied.Decode(&e); err != nil {
		t.Fatalf("解码 error: %v", err)
	}
	if e.Code != protocol.CodeForbidden {
		t.Fatalf("错误码 = %s，期望 forbidden", e.Code)
	}

	send(t, ops, protocol.TypeSessionManage, protocol.SessionManagePayload{SessionID: id})
	list := readEnvelope(t, ops)
	var after protocol.SessionManageResultPayload
	if err := list.Decode(&after); err != nil {
		t.Fatalf("解码会话列表: %v", err)
	}
	if len(after.Sessions) != 1 || after.Sessions[0].Title != "只能看" {
		t.Fatalf("标题不该被改动：%+v", after.Sessions)
	}
}
