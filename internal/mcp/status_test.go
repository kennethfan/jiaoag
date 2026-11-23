package mcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	"github.com/kennethfan/jiaoag/internal/project"
)

func TestStatusBeforeConnectShowsConfiguredServers(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Argv: []string{"npx", "fs-server"}, Timeout: time.Second})
	status := m.Status("/repo", nil)
	if len(status) != 1 {
		t.Fatalf("server 数 = %d，期望 1", len(status))
	}
	got := status[0]
	if got.Name != "fs" || got.Source != "config" {
		t.Fatalf("状态 = %+v", got)
	}
	// 只看一眼不该把进程拉起来：/mcp 是只读快照。
	if got.Connected || got.Tools != 0 || got.Version != "" {
		t.Fatalf("未连接时不该有运行信息：%+v", got)
	}
	if strings.Join(got.Argv, " ") != "npx fs-server" {
		t.Fatalf("argv = %v", got.Argv)
	}
}

func TestStatusAfterConnectReportsToolsAndVersion(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Timeout: time.Second})
	_ = m.Tools(context.Background(), "/repo")

	status := m.Status("/repo", nil)
	if len(status) != 1 {
		t.Fatalf("server 数 = %d", len(status))
	}
	got := status[0]
	if !got.Connected || got.Tools != 1 {
		t.Fatalf("连接后应报告 1 个工具：%+v", got)
	}
	if got.Version != "9.9.9" {
		t.Fatalf("版本 = %q，期望从工具版本串里解析出 9.9.9", got.Version)
	}
}

func TestStatusMarksProjectServersAndOverridesByName(t *testing.T) {
	m, _ := managerWith(t, mcppkg.Server{Name: "fs", Argv: []string{"global-fs"}, Timeout: time.Second})
	extra := []mcppkg.Server{{Name: "fs", Argv: []string{"repo-fs"}, Timeout: 2 * time.Second}}
	_ = m.ToolsWith(context.Background(), "/repo", extra)

	status := m.Status("/repo", extra)
	if len(status) != 1 {
		t.Fatalf("同名应合并成一台，实际 %d 台", len(status))
	}
	got := status[0]
	// 项目级清单是仓库作者的明确要求：同名时覆盖全局，不是被全局压掉。
	if strings.Join(got.Argv, " ") != "repo-fs" || got.Source != "project" {
		t.Fatalf("项目级配置未覆盖全局：%+v", got)
	}
	if got.Timeout != 2*time.Second {
		t.Fatalf("timeout = %v，期望沿用项目级的 2s", got.Timeout)
	}
}

func TestServersFromProjectCopiesFields(t *testing.T) {
	list := mcppkg.ServersFromProject([]project.Server{{
		Name:    "repo",
		Argv:    []string{"echo", "hi"},
		Env:     []string{"A=B"},
		Dir:     "/repo",
		Timeout: 3 * time.Second,
		Trust:   true,
	}})
	if len(list) != 1 {
		t.Fatalf("翻译结果 = %d 台", len(list))
	}
	got := list[0]
	if got.Name != "repo" || len(got.Argv) != 2 || len(got.Env) != 1 || got.Dir != "/repo" ||
		got.Timeout != 3*time.Second || !got.Trust {
		t.Fatalf("字段丢失：%+v", got)
	}
}
