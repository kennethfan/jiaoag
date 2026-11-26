package boot_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/boot"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
)

const testConfig = `server:
  listen: 127.0.0.1:0
  token: test-token
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
`

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := strings.NewReplacer(
		"%DB%", filepath.Join(dir, "data", "jiaoag.db"),
		"%OUT%", filepath.Join(dir, "out"),
		"%POLICY%", filepath.Join(dir, "policy.d"),
	).Replace(testConfig)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置: %v", err)
	}
	return path
}

func open(t *testing.T, cfgPath string) *boot.Stack {
	t.Helper()
	stack, err := boot.Open(context.Background(), boot.Options{
		CfgPath: cfgPath,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot.Open: %v", err)
	}
	return stack
}

// 同一数据目录只允许一个运行时：内嵌 TUI 与 serve 不能同时写同一个库。
func TestOpenExcludesSecondRuntime(t *testing.T) {
	cfgPath := writeConfig(t)
	first := open(t, cfgPath)
	defer first.Close()

	_, err := boot.Open(context.Background(), boot.Options{
		CfgPath: cfgPath,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if !errors.Is(err, boot.ErrLocked) {
		t.Fatalf("第二个运行时应当拿到 ErrLocked，实际 %v", err)
	}

	first.Close()
	second := open(t, cfgPath)
	second.Close()
}

// 内嵌模式下 TUI 真的能拿到契约与事件流，不依赖端口。
func TestOpenServesSessionsWithoutNetwork(t *testing.T) {
	stack := open(t, writeConfig(t))
	defer stack.Close()

	ctx := context.Background()
	sess, err := stack.Core.SessionCreate(ctx, rpc.Owner(), protocol.SessionCreatePayload{
		Workspace: t.TempDir(),
		Title:     "内嵌",
	})
	if err != nil {
		t.Fatalf("SessionCreate: %v", err)
	}
	list, err := stack.Core.SessionManage(ctx, rpc.Owner(), protocol.SessionManagePayload{All: true})
	if err != nil {
		t.Fatalf("SessionManage: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != sess.ID {
		t.Fatalf("会话清单不对: %+v", list.Sessions)
	}

	frames, cancel, err := stack.Streams.Subscribe(ctx, protocol.EventSubscribePayload{SessionID: sess.ID, FromSeq: 0})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	env, ok := <-frames
	if !ok {
		t.Fatal("事件流提前关闭")
	}
	if env.Type != protocol.TypeEvent {
		t.Fatalf("首帧类型 %s", env.Type)
	}
	if env.SessionID != sess.ID {
		t.Fatalf("事件帧缺少会话号: %+v", env)
	}
}
