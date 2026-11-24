package server

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// 单次模型覆盖的 provider 必须在配置里真实存在：拼错时宁可报错，
// 也不要回落到会话默认模型后悄悄跑另一套。
func TestTaskSubmitRejectsUnknownTaskProvider(t *testing.T) {
	ts := newModelsServer(t, config.Provider{Name: "agnes", Type: "openai", BaseURL: "http://127.0.0.1:1/v1", Model: "agnes-3.0-flash"})
	c, sessID := modelsSession(t, ts)

	env, err := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{
		SessionID: sessID,
		Text:      "用未知供应商跑",
		Provider:  "no-such",
		Model:     "m1",
	})
	if err != nil {
		t.Fatalf("构造 task.submit: %v", err)
	}
	write(t, c, env)

	reply := readEnvelope(t, c)
	if reply.Type != protocol.TypeError {
		t.Fatalf("回帧类型 = %s，期望 error", reply.Type)
	}
	var p protocol.ErrorPayload
	if err := reply.Decode(&p); err != nil {
		t.Fatalf("解码错误帧: %v", err)
	}
	if p.Code != protocol.CodeBadRequest || !strings.Contains(p.Message, "no-such") {
		t.Fatalf("错误内容不符：%+v", p)
	}
}

func TestTaskSubmitAcceptsKnownTaskModel(t *testing.T) {
	ts := newModelsServer(t, config.Provider{Name: "agnes", Type: "openai", BaseURL: "http://127.0.0.1:1/v1", Model: "agnes-3.0-flash"})
	c, sessID := modelsSession(t, ts)

	env, err := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{
		SessionID: sessID,
		Text:      "用指定模型跑",
		Provider:  "agnes",
		Model:     "agnes-2.5-flash",
	})
	if err != nil {
		t.Fatalf("构造 task.submit: %v", err)
	}
	write(t, c, env)

	reply := readEnvelope(t, c)
	if reply.Type != protocol.TypeTaskQueued {
		t.Fatalf("回帧类型 = %s，期望 task.queued", reply.Type)
	}
}
