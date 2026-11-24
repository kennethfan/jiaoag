package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// handler 处理一个请求，返回 result；errMsg 非空时回一个 RPC 错误。
type handler func(method string, params json.RawMessage) (any, string)

// fakeDial 起一个进程内插件：net.Pipe 的两端分别给客户端与桩服务端。
func fakeDial(t *testing.T, h handler) (func(context.Context, Server) (io.ReadWriteCloser, error), *int32) {
	t.Helper()
	var calls int32
	dial := func(ctx context.Context, _ Server) (io.ReadWriteCloser, error) {
		atomic.AddInt32(&calls, 1)
		clientEnd, serverEnd := net.Pipe()
		go serve(serverEnd, h)
		t.Cleanup(func() { clientEnd.Close() })
		return clientEnd, nil
	}
	return dial, &calls
}

func serve(rwc io.ReadWriteCloser, h handler) {
	defer rwc.Close()
	in := bufio.NewReaderSize(rwc, 1<<16)
	enc := json.NewEncoder(rwc)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		out := response{JSONRPC: "2.0", ID: req.ID}
		res, msg := h(req.Method, req.Params)
		if msg != "" {
			out.Error = &rpcError{Code: -32000, Message: msg}
		} else {
			raw, err := json.Marshal(res)
			if err != nil {
				return
			}
			out.Result = raw
		}
		if err := enc.Encode(out); err != nil {
			return
		}
	}
}

func echoHandler(kind PrepareResult, content string) handler {
	return func(method string, params json.RawMessage) (any, string) {
		switch method {
		case methodInitialize:
			return Info{Name: "demo", Version: "1.2.3", ProtocolVersion: ProtocolVersion}, ""
		case methodListTools:
			return map[string]any{"tools": []ToolSpec{{
				Name:        "echo",
				Description: "测试工具",
				Schema:      json.RawMessage(`{"type":"object"}`),
			}}}, ""
		case methodPrepare:
			return kind, ""
		case methodExecute:
			return ExecuteResult{Content: content}, ""
		}
		return nil, "未知方法 " + method
	}
}

func testWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

func testEnv() tools.RuntimeEnv {
	return tools.RuntimeEnv{
		Backend:    execpkg.NewDirect(),
		Timeout:    10 * time.Second,
		EventLimit: 8 << 10,
		LogLimit:   1 << 20,
	}
}

func toolsOf(t *testing.T, dial func(context.Context, Server) (io.ReadWriteCloser, error), ws *workspace.Workspace) []tools.Tool {
	t.Helper()
	m := NewManager(Options{
		Servers: []Server{{Name: "demo", Argv: []string{"demo"}}},
		Version: "test",
		Dial:    dial,
	})
	t.Cleanup(func() { m.Close() })
	return m.Tools(context.Background(), ws, testEnv())
}

func TestManagerListsAndCachesTools(t *testing.T) {
	dial, calls := fakeDial(t, echoHandler(PrepareResult{Kind: KindRead}, "ok"))
	ws := testWorkspace(t)

	m := NewManager(Options{
		Servers: []Server{{Name: "demo", Argv: []string{"demo"}}},
		Version: "test",
		Dial:    dial,
	})
	t.Cleanup(func() { m.Close() })

	list := m.Tools(context.Background(), ws, testEnv())
	if len(list) != 1 {
		t.Fatalf("工具数 %d，期望 1", len(list))
	}
	spec := list[0].Spec()
	if spec.Name != "plugin__demo__echo" {
		t.Errorf("工具名 %q", spec.Name)
	}
	if spec.Source != "plugin:demo" {
		t.Errorf("来源 %q", spec.Source)
	}
	if !strings.HasPrefix(spec.Version, "plugin:demo@1.2.3#") {
		t.Errorf("版本 %q", spec.Version)
	}
	// 同一个 root 再取一次不应该重连。
	_ = m.Tools(context.Background(), ws, testEnv())
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("连接次数 %d，期望 1", got)
	}
}

func TestPrepareReadThenExecute(t *testing.T) {
	dial, _ := fakeDial(t, echoHandler(PrepareResult{Kind: KindRead}, "来自插件"))
	list := toolsOf(t, dial, testWorkspace(t))

	prep, err := list[0].Prepare(context.Background(), tools.Call{
		ID: "call_1", Name: "plugin__demo__echo", Args: json.RawMessage(`{"text":"hi"}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Change != nil || prep.Command != nil || prep.External != nil {
		t.Fatalf("只读工具不应带副作用: %+v", prep)
	}
	res, err := list[0].Execute(context.Background(), prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Content != "来自插件" {
		t.Errorf("内容 %q", res.Content)
	}
}

func TestPrepareWriteBuildsChangeWithoutWriting(t *testing.T) {
	ws := testWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws.Root(), "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write("notes/a.md", []byte("旧内容\n")); err != nil {
		t.Fatal(err)
	}
	dial, _ := fakeDial(t, echoHandler(PrepareResult{
		Kind: KindWrite, Path: "notes/a.md", Content: "新内容\n",
	}, ""))
	list := toolsOf(t, dial, ws)

	prep, err := list[0].Prepare(context.Background(), tools.Call{ID: "call_1", Name: "plugin__demo__echo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Change == nil {
		t.Fatal("期望产生 Change")
	}
	if prep.Change.Path != "notes/a.md" || !strings.Contains(prep.Change.Diff, "-旧内容") {
		t.Errorf("Change 不对: %+v", prep.Change)
	}
	if prep.Change.BaseHash == "" || prep.Change.NewHash == "" {
		t.Error("hash 应该都填上")
	}
	// 宿主还没审批，磁盘不能被改。
	got, err := os.ReadFile(filepath.Join(ws.Root(), "notes/a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "旧内容\n" {
		t.Errorf("文件被提前写入：%q", got)
	}
}

func TestPrepareWriteRejectsEscape(t *testing.T) {
	dial, _ := fakeDial(t, echoHandler(PrepareResult{Kind: KindWrite, Path: "../escape.txt", Content: "x"}, ""))
	list := toolsOf(t, dial, testWorkspace(t))
	if _, err := list[0].Prepare(context.Background(), tools.Call{ID: "c", Name: "plugin__demo__echo"}); err == nil {
		t.Fatal("越界路径应该报错")
	}
}

func TestPrepareCommandUsesHostCwd(t *testing.T) {
	ws := testWorkspace(t)
	dial, _ := fakeDial(t, echoHandler(PrepareResult{
		Kind: KindCommand, Argv: []string{"echo", "hi"}, Cwd: "/tmp", Timeout: intPtr(5),
	}, ""))
	list := toolsOf(t, dial, ws)

	prep, err := list[0].Prepare(context.Background(), tools.Call{ID: "c", Name: "plugin__demo__echo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Command == nil {
		t.Fatal("期望产生 Command")
	}
	if prep.Command.Cwd != ws.Root() {
		t.Errorf("cwd = %q，应为工作区根", prep.Command.Cwd)
	}
	if prep.Command.Backend != "direct" {
		t.Errorf("backend = %q", prep.Command.Backend)
	}
	if prep.Command.Timeout != 5*time.Second {
		t.Errorf("timeout = %s", prep.Command.Timeout)
	}
}

func TestCommandExecutesThroughBackend(t *testing.T) {
	ws := testWorkspace(t)
	dial, _ := fakeDial(t, echoHandler(PrepareResult{Kind: KindCommand, Argv: []string{"echo", "plugin-cmd"}}, ""))
	list := toolsOf(t, dial, ws)

	prep, err := list[0].Prepare(context.Background(), tools.Call{ID: "c", Name: "plugin__demo__echo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := list[0].Execute(context.Background(), prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "plugin-cmd") || res.IsError {
		t.Errorf("结果 %+v", res)
	}
}

func TestExternalRequiresApproval(t *testing.T) {
	dial, _ := fakeDial(t, echoHandler(PrepareResult{Kind: KindExternal, Summary: "插件自己跑"}, "pong"))
	list := toolsOf(t, dial, testWorkspace(t))

	prep, err := list[0].Prepare(context.Background(), tools.Call{ID: "c", Name: "plugin__demo__echo"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.External == nil {
		t.Fatal("期望 External")
	}
	if prep.External.Source != "plugin:demo" || prep.External.Trusted {
		t.Errorf("External = %+v", prep.External)
	}
	res, err := list[0].Execute(context.Background(), prep)
	if err != nil || res.Content != "pong" {
		t.Fatalf("Execute: %v %+v", err, res)
	}
}

func TestUnknownKindFails(t *testing.T) {
	dial, _ := fakeDial(t, echoHandler(PrepareResult{Kind: "teleport"}, ""))
	list := toolsOf(t, dial, testWorkspace(t))
	if _, err := list[0].Prepare(context.Background(), tools.Call{ID: "c", Name: "plugin__demo__echo"}); err == nil {
		t.Fatal("未知副作用类型应该报错")
	}
}

func TestUnavailablePluginIsSkipped(t *testing.T) {
	dial := func(context.Context, Server) (io.ReadWriteCloser, error) {
		return nil, errors.New("起不来")
	}
	ws := testWorkspace(t)
	m := NewManager(Options{
		Servers: []Server{{Name: "bad", Argv: []string{"bad"}}, {Name: "demo", Argv: []string{"demo"}}},
		Dial:    dial,
	})
	t.Cleanup(func() { m.Close() })
	if list := m.Tools(context.Background(), ws, testEnv()); len(list) != 0 {
		t.Fatalf("插件不可用时不应有工具，得到 %d", len(list))
	}
}

func TestProtocolVersionMismatch(t *testing.T) {
	dial, _ := fakeDial(t, func(method string, _ json.RawMessage) (any, string) {
		if method == methodInitialize {
			return Info{Name: "demo", Version: "1", ProtocolVersion: 99}, ""
		}
		return nil, ""
	})
	ws := testWorkspace(t)
	m := NewManager(Options{Servers: []Server{{Name: "demo", Argv: []string{"demo"}}}, Dial: dial})
	t.Cleanup(func() { m.Close() })
	if list := m.Tools(context.Background(), ws, testEnv()); len(list) != 0 {
		t.Fatalf("协议版本不符应跳过，得到 %d 个工具", len(list))
	}
}

func TestDisplayNameTruncatesLongNames(t *testing.T) {
	name := displayName("server-with-a-quite-long-name", strings.Repeat("tool", 20))
	if len(name) > nameMaxBytes {
		t.Fatalf("名字 %d 字节", len(name))
	}
	if !strings.HasPrefix(name, "plugin__") {
		t.Errorf("前缀不对: %q", name)
	}
}

func intPtr(v int) *int { return &v }

func TestErrorFromPluginBecomesToolError(t *testing.T) {
	dial, _ := fakeDial(t, func(method string, _ json.RawMessage) (any, string) {
		switch method {
		case methodInitialize:
			return Info{Name: "demo", Version: "1", ProtocolVersion: ProtocolVersion}, ""
		case methodListTools:
			return map[string]any{"tools": []ToolSpec{{Name: "echo", Description: "", Schema: json.RawMessage(`{}`)}}}, ""
		case methodPrepare:
			return nil, "准备失败"
		}
		return nil, ""
	})
	list := toolsOf(t, dial, testWorkspace(t))
	_, err := list[0].Prepare(context.Background(), tools.Call{ID: "c", Name: "plugin__demo__echo"})
	if err == nil || !strings.Contains(err.Error(), "准备失败") {
		t.Fatalf("错误未透传: %v", err)
	}
}
