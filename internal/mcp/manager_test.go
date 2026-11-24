package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// fakeSession 起一台进程内的 MCP server，覆盖文本、非文本、错误与慢调用。
func fakeSession(t *testing.T) *sdk.ClientSession {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "9.9.9"}, nil)
	srv.AddTool(&sdk.Tool{
		Name:        "echo",
		Description: "echo text",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var in struct {
			Text string `json:"text"`
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, err
		}
		switch in.Mode {
		case "image":
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
			}}, nil
		case "fail":
			return &sdk.CallToolResult{
				Content: []sdk.Content{&sdk.TextContent{Text: "boom"}},
				IsError: true,
			}, nil
		case "slow":
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo:" + in.Text}}}, nil
	})

	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func managerWith(t *testing.T, servers ...mcppkg.Server) (*mcppkg.Manager, *int32) {
	t.Helper()
	var connects int32
	m := mcppkg.NewManager(mcppkg.Options{
		Servers: servers,
		Logger:  nil,
		Connect: func(context.Context, mcppkg.Server) (*sdk.ClientSession, error) {
			atomic.AddInt32(&connects, 1)
			return fakeSession(t), nil
		},
	})
	t.Cleanup(func() { _ = m.Close() })
	return m, &connects
}

// TestManagerSourceImplementsToolsSource 保证 MCP 能和内置工具走同一套
// 来源聚合，而不是让宿主为它写专门的装配代码。
func TestManagerSourceImplementsToolsSource(t *testing.T) {
	m, connects := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	src := m.Source("/repo", nil)
	if src.Name() != "mcp" {
		t.Fatalf("来源名 = %q，期望 mcp", src.Name())
	}
	reg, errs := tools.NewRegistryFrom(context.Background(), src)
	if len(errs) != 0 {
		t.Fatalf("聚合出错：%v", errs)
	}
	spec, ok := reg.Get("mcp__fs__echo")
	if !ok {
		t.Fatalf("工具没有注册：%v", reg.Names())
	}
	if spec.Spec().Source != "mcp:fs" {
		t.Fatalf("Source = %q，期望保留 mcp:fs", spec.Spec().Source)
	}
	// Close 是空操作：会话归 Manager 所有，装配结束不能杀进程。
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reg2, errs := tools.NewRegistryFrom(context.Background(), m.Source("/repo", nil))
	if len(errs) != 0 {
		t.Fatalf("Close 后聚合出错：%v", errs)
	}
	if _, ok := reg2.Get("mcp__fs__echo"); !ok {
		t.Fatal("Close 之后工具消失了，说明来源误关共享会话")
	}
	if got := atomic.LoadInt32(connects); got != 1 {
		t.Fatalf("connect 次数 = %d，期望 1（同一 workspace 复用会话）", got)
	}
}

func TestManagerListsToolsAndNamesThemBySource(t *testing.T) {
	m, connects := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	list := m.Tools(context.Background(), "/repo")
	if len(list) != 1 {
		t.Fatalf("工具数 = %d，期望 1", len(list))
	}
	spec := list[0].Spec()
	if spec.Name != "mcp__fs__echo" {
		t.Fatalf("工具名 = %q", spec.Name)
	}
	if len(spec.Schema) == 0 || !strings.Contains(string(spec.Schema), `"required"`) {
		t.Fatalf("schema 丢失：%s", spec.Schema)
	}
	if !strings.Contains(spec.Version, "mcp:fs@9.9.9#") {
		t.Fatalf("版本 = %q，期望包含 server 版本与 schema hash", spec.Version)
	}
	if len(spec.Effects) != 1 || spec.Effects[0] != tools.EffectProcess {
		t.Fatalf("effects = %v，期望 process", spec.Effects)
	}
	if spec.Source != "mcp:fs" {
		t.Fatalf("Source = %q，期望 mcp:fs（审计与面板靠它定位来源）", spec.Source)
	}
	// 同一 workspace 只连一次：每个任务都会调用 Tools()。
	_ = m.Tools(context.Background(), "/repo")
	if got := atomic.LoadInt32(connects); got != 1 {
		t.Fatalf("connect 次数 = %d，期望 1", got)
	}
	// 不同 workspace 各自一份缓存。
	_ = m.Tools(context.Background(), "/other")
	if got := atomic.LoadInt32(connects); got != 2 {
		t.Fatalf("不同 workspace 的 connect 次数 = %d，期望 2", got)
	}
}

func TestManagerSkipsUnavailableServer(t *testing.T) {
	var connects int32
	m := mcppkg.NewManager(mcppkg.Options{
		Servers: []mcppkg.Server{{Name: "bad", Timeout: time.Second}, {Name: "fs", Timeout: time.Second}},
		Connect: func(_ context.Context, s mcppkg.Server) (*sdk.ClientSession, error) {
			atomic.AddInt32(&connects, 1)
			if s.Name == "bad" {
				return nil, errors.New("boom")
			}
			return fakeSession(t), nil
		},
	})
	t.Cleanup(func() { _ = m.Close() })

	list := m.Tools(context.Background(), "/repo")
	if len(list) != 1 || list[0].Spec().Name != "mcp__fs__echo" {
		t.Fatalf("不可用的 server 应被跳过，实际 %+v", specsOf(list))
	}
	if atomic.LoadInt32(&connects) != 2 {
		t.Fatalf("两台 server 都应尝试连接")
	}
}

func TestPrepareReportsMissingRequiredArgument(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	tool := m.Tools(context.Background(), "/repo")[0]

	_, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: tool.Spec().Name, Args: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "text") {
		t.Fatalf("缺 required 参数应报错，得到 %v", err)
	}
	if _, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: tool.Spec().Name, Args: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("非 object 参数应报错")
	}
}

func TestExecuteRendersContentAndErrors(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	tool := m.Tools(context.Background(), "/repo")[0]

	prep, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: tool.Spec().Name, Args: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if prep.External == nil || prep.External.Source != "mcp:fs" || prep.External.Trusted {
		t.Fatalf("Prepared.External = %+v", prep.External)
	}
	res, err := tool.Execute(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "echo:hi") {
		t.Fatalf("结果 = %+v", res)
	}

	prep, _ = tool.Prepare(context.Background(), tools.Call{ID: "c2", Name: tool.Spec().Name, Args: json.RawMessage(`{"text":"x","mode":"image"}`)})
	res, err = tool.Execute(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "image/png") || strings.Contains(res.Content, "\x01") {
		t.Fatalf("非文本内容应占位且不泄露二进制：%q", res.Content)
	}

	prep, _ = tool.Prepare(context.Background(), tools.Call{ID: "c3", Name: tool.Spec().Name, Args: json.RawMessage(`{"text":"x","mode":"fail"}`)})
	res, err = tool.Execute(context.Background(), prep)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "boom") {
		t.Fatalf("工具自报错误应透传：%+v", res)
	}
}

func TestTrustedServerMarksExternalTrusted(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second, Trust: true})
	tool := m.Tools(context.Background(), "/repo")[0]
	prep, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: tool.Spec().Name, Args: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if prep.External == nil || !prep.External.Trusted {
		t.Fatalf("trust: allow 的 server 应标记 Trusted：%+v", prep.External)
	}
}

func TestExecuteTimeoutIsToolError(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Timeout: 20 * time.Millisecond})
	tool := m.Tools(context.Background(), "/repo")[0]
	prep, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: tool.Spec().Name, Args: json.RawMessage(`{"text":"x","mode":"slow"}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), prep)
	if err != nil {
		t.Fatalf("超时是工具错误，不该让任务失败：%v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "超时") {
		t.Fatalf("超时结果 = %+v", res)
	}
}

func TestExecuteParentCancelReturnsContextError(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Timeout: 5 * time.Second})
	tool := m.Tools(context.Background(), "/repo")[0]
	prep, err := tool.Prepare(context.Background(), tools.Call{ID: "c1", Name: tool.Spec().Name, Args: json.RawMessage(`{"text":"x","mode":"slow"}`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, prep)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消应返回 context.Canceled，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消没有及时返回")
	}
}

func specsOf(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Spec().Name)
	}
	return out
}

// 项目级 server 与全局 server 合并：同一个 registry 里两套工具。
func TestManagerMergesProjectServers(t *testing.T) {
	m, connects := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	extra := []mcppkg.Server{{Name: "repo", Timeout: time.Second}}

	list := m.ToolsWith(context.Background(), "/repo", extra)
	names := specsOf(list)
	if len(names) != 2 {
		t.Fatalf("应有两台 server 的工具，实际 %v", names)
	}
	for _, want := range []string{"mcp__fs__echo", "mcp__repo__echo"} {
		if !contains(names, want) {
			t.Fatalf("缺少 %s：%v", want, names)
		}
	}
	if got := atomic.LoadInt32(connects); got != 2 {
		t.Fatalf("connect 次数 = %d，期望 2", got)
	}
}

// 项目级清单变了（增删 server 或改 argv）必须重建，否则仓库里改配置要重启才生效。
func TestManagerRebuildsWhenProjectServersChange(t *testing.T) {
	m, connects := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	ctx := context.Background()

	_ = m.ToolsWith(ctx, "/repo", []mcppkg.Server{{Name: "repo", Timeout: time.Second}})
	if got := atomic.LoadInt32(connects); got != 2 {
		t.Fatalf("首轮 connect 次数 = %d，期望 2", got)
	}

	// 同样的清单：沿用缓存，不重连。
	_ = m.ToolsWith(ctx, "/repo", []mcppkg.Server{{Name: "repo", Timeout: time.Second}})
	if got := atomic.LoadInt32(connects); got != 2 {
		t.Fatalf("清单未变不该重连，实际 %d", got)
	}

	// 清单变化：重建该工作区的全部 server。
	list := m.ToolsWith(ctx, "/repo", nil)
	if got := atomic.LoadInt32(connects); got != 3 {
		t.Fatalf("清单变化后应重建（只连全局那台），实际 %d", got)
	}
	if names := specsOf(list); len(names) != 1 || names[0] != "mcp__fs__echo" {
		t.Fatalf("重建后只应剩全局工具，实际 %v", names)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
