package tui

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/boot"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
)

func testStack(t *testing.T) *boot.Stack {
	t.Helper()
	dir := t.TempDir()
	body := strings.NewReplacer(
		"%DB%", filepath.Join(dir, "d", "jiaoag.db"),
		"%OUT%", filepath.Join(dir, "out"),
		"%POLICY%", filepath.Join(dir, "policy.d"),
	).Replace(`server:
  listen: 127.0.0.1:0
  token: t
storage:
  db_path: %DB%
  output_dir: %OUT%
policy:
  dir: %POLICY%
providers:
  - name: p
    type: openai
    base_url: http://127.0.0.1:1/v1
    model: m
    api_key: k
`)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置: %v", err)
	}
	stack, err := boot.Open(context.Background(), boot.Options{
		CfgPath: path,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot.Open: %v", err)
	}
	t.Cleanup(stack.Close)
	return stack
}

func readType(t *testing.T, tr Transport, want string) protocol.Envelope {
	t.Helper()
	done := make(chan protocol.Envelope, 1)
	go func() {
		env, err := tr.Read()
		if err == nil {
			done <- env
		}
	}()
	select {
	case env := <-done:
		if env.Type != want {
			t.Fatalf("期望 %s，收到 %s（%s）", want, env.Type, env.Payload)
		}
		return env
	case <-time.After(3 * time.Second):
		t.Fatalf("等待 %s 超时", want)
		return protocol.Envelope{}
	}
}

// 内嵌传输不经过端口：一帧进、一帧出，形状与 WebSocket 完全一致。
func TestLocalTransportProjectsFrames(t *testing.T) {
	stack := testStack(t)
	tr := NewLocalTransport(stack.Core, stack.Streams, rpc.Owner())
	defer tr.Close()

	create, err := protocol.New(protocol.TypeSessionCreate, protocol.SessionCreatePayload{
		Workspace: t.TempDir(),
		Title:     "本地",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(create); err != nil {
		t.Fatalf("Send: %v", err)
	}
	env := readType(t, tr, protocol.TypeSessionCreated)
	var sess protocol.SessionPayload
	if err := env.Decode(&sess); err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" || env.SessionID != sess.ID {
		t.Fatalf("回执会话号不对: %+v", env)
	}

	manage, err := protocol.New(protocol.TypeSessionManage, protocol.SessionManagePayload{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(manage); err != nil {
		t.Fatal(err)
	}
	list := readType(t, tr, protocol.TypeSessionManageResult)
	var res protocol.SessionManageResultPayload
	if err := list.Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].ID != sess.ID {
		t.Fatalf("清单不对: %+v", res.Sessions)
	}

	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{SessionID: sess.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(sub); err != nil {
		t.Fatal(err)
	}
	event := readType(t, tr, protocol.TypeEvent)
	if event.SessionID != sess.ID {
		t.Fatalf("事件缺少会话号: %+v", event)
	}
}

// 错误走 error 帧，和 WS 客户端看到的形状一致。
func TestLocalTransportReportsErrors(t *testing.T) {
	stack := testStack(t)
	tr := NewLocalTransport(stack.Core, stack.Streams, rpc.Owner())
	defer tr.Close()

	bad, err := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{SessionID: "ses_missing", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(bad); err != nil {
		t.Fatalf("Send: %v", err)
	}
	env := readType(t, tr, protocol.TypeError)
	var e protocol.ErrorPayload
	if err := env.Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Code == "" || e.Message == "" {
		t.Fatalf("错误帧不完整: %+v", e)
	}
}
