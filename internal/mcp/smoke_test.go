//go:build mcpsmoke

// 真实 MCP server 冒烟：验证 CommandTransport、tools/list 与 tools/call 的
// 完整链路。需要本机 npx 与网络（首次会下载 server），因此不进 make check。
//
//	go test -tags mcpsmoke -run TestMcpFilesystemSmoke -v ./internal/mcp/
package mcp_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	"github.com/kennethfan/jiaoag/internal/tools"
)

func TestMcpFilesystemSmoke(t *testing.T) {
	dir := t.TempDir()
	want := "hello from mcp smoke\n"
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := mcppkg.NewManager(mcppkg.Options{
		Servers: []mcppkg.Server{{
			Name:    "fs",
			Argv:    []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", dir},
			Trust:   false,
			Timeout: 60 * time.Second,
		}},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	defer mgr.Close()

	ts := mgr.Tools(context.Background(), dir)
	if len(ts) == 0 {
		t.Fatal("没有列出任何工具：server 可能未启动")
	}
	byName := make(map[string]tools.Tool, len(ts))
	for _, tl := range ts {
		byName[tl.Spec().Name] = tl
	}
	read, ok := byName["mcp__fs__read_text_file"]
	if !ok {
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		t.Fatalf("缺少 read_text_file，实际工具：%s", strings.Join(names, ", "))
	}
	if got := read.Spec().Version; !strings.HasPrefix(got, "mcp:fs@") {
		t.Fatalf("Version = %q，期望以 mcp:fs@ 开头", got)
	}

	args, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "hello.txt")})
	prepared, err := read.Prepare(context.Background(), tools.Call{ID: "call_smoke", Name: read.Spec().Name, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.External == nil || prepared.External.Source != "mcp:fs" {
		t.Fatalf("Prepared.External = %+v", prepared.External)
	}

	res, err := read.Execute(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("调用失败：%s", res.Content)
	}
	if !strings.Contains(res.Content, strings.TrimSpace(want)) {
		t.Fatalf("结果不含文件内容：%q", res.Content)
	}
}
