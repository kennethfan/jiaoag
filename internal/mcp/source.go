package mcp

import (
	"context"

	"github.com/kennethfan/jiaoag/internal/tools"
)

// workspaceSource 是某个工作区的 MCP 工具视图，实现 tools.Source。
//
// 会话（子进程）归 Manager 所有：按 root 缓存、跨任务复用、退出时统一由
// Manager.Close 回收。所以这里的 Close 是空操作——注册表是按任务装配的，
// 不能在装配结束时顺手杀掉别的任务还在用的进程。
type workspaceSource struct {
	m     *Manager
	root  string
	extra []Server
}

// Source 返回该工作区的 MCP 工具视图，交给宿主聚合进 tools.Registry。
func (m *Manager) Source(workspaceRoot string, extra []Server) tools.Source {
	return &workspaceSource{m: m, root: workspaceRoot, extra: extra}
}

// Name 是粗粒度来源名；单个工具的 Spec.Source 精确到 mcp:<server>。
func (s *workspaceSource) Name() string { return "mcp" }

// Tools 不会失败：单台 server 起不来时 Manager 只记日志，返回其余工具。
// 这是「插件不得成为运行前提」的具体体现（ADR-0009）。
func (s *workspaceSource) Tools(ctx context.Context) ([]tools.Tool, error) {
	return s.m.ToolsWith(ctx, s.root, s.extra), nil
}

// Close 由 Manager.Close 统一负责，见类型注释。
func (s *workspaceSource) Close() error { return nil }
