package mcp

import (
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/project"
)

// ServerStatus 是一台 MCP server 的配置与运行状态快照。
//
// 它只描述「宿主自己知道的事」：起了没有、列到几个工具、server 报的版本。
// 不发起连接、也不触发启动——/mcp 面板不该因为用户看了一眼就把进程拉起来。
type ServerStatus struct {
	Name    string
	Argv    []string
	Dir     string
	Trust   bool
	Timeout time.Duration
	// Source 是这台的来源：config（全局配置）或 project（工作区的 .jiaoag/mcp.yaml）。
	Source string
	// Connected 表示该工作区里它的子进程已经连上过。false 可能是还没用到，
	// 也可能是启动失败被跳过，两者在这里不区分。
	Connected bool
	// Version 是 server 在 initialize 时自报的版本。
	Version string
	// Tools 是它暴露的工具数量。
	Tools int
}

// Status 报告该工作区配置的 server 与各自状态。extra 是项目级 server，
// 与 ToolsWith 用同一套合并规则，所以面板显示的就是真正会跑的那一组。
func (m *Manager) Status(root string, extra []Server) []ServerStatus {
	merged := mergeServers(m.servers, extra)
	if len(merged) == 0 {
		return nil
	}
	projectNames := make(map[string]bool, len(extra))
	for _, s := range extra {
		projectNames[s.Name] = true
	}

	m.mu.Lock()
	state := m.roots[root]
	m.mu.Unlock()
	var runtime []ServerStatus
	if state != nil {
		state.mu.Lock()
		runtime = append(runtime, state.status...)
		state.mu.Unlock()
	}
	byName := make(map[string]ServerStatus, len(runtime))
	for _, s := range runtime {
		byName[s.Name] = s
	}

	out := make([]ServerStatus, 0, len(merged))
	for _, srv := range merged {
		st := ServerStatus{
			Name:    srv.Name,
			Argv:    srv.Argv,
			Dir:     srv.Dir,
			Trust:   srv.Trust,
			Timeout: srv.Timeout,
			Source:  "config",
		}
		if projectNames[srv.Name] {
			st.Source = "project"
		}
		if live, ok := byName[srv.Name]; ok {
			st.Connected = live.Connected
			st.Version = live.Version
			st.Tools = live.Tools
		}
		out = append(out, st)
	}
	return out
}

// mergeServers 合并全局与项目级 server，同名时后者覆盖前者（仓库对自己的
// MCP 有最终发言权），同时保持首次出现的顺序。
//
// 覆盖而不是「先到先得」：项目级清单是仓库作者对这台 server 的明确要求，
// 静默忽略它会让「我在 mcp.yaml 里改的参数没生效」变成一桩悬案。
func mergeServers(global, extra []Server) []Server {
	out := make([]Server, 0, len(global)+len(extra))
	index := make(map[string]int, len(global)+len(extra))
	for _, srv := range global {
		index[srv.Name] = len(out)
		out = append(out, srv)
	}
	for _, srv := range extra {
		if i, ok := index[srv.Name]; ok {
			out[i] = srv
			continue
		}
		index[srv.Name] = len(out)
		out = append(out, srv)
	}
	return out
}

// ServersFromProject 把项目级声明翻译成启动描述。
//
// 两边字段一一对应，但类型分属两个包：project 只知道 yaml 长什么样，
// mcp 只管怎么起进程，翻译放在这里，两边都不用互相认识。
func ServersFromProject(list []project.Server) []Server {
	out := make([]Server, 0, len(list))
	for _, srv := range list {
		out = append(out, Server{
			Name:    srv.Name,
			Argv:    srv.Argv,
			Env:     srv.Env,
			Dir:     srv.Dir,
			Timeout: srv.Timeout,
			Trust:   srv.Trust,
		})
	}
	return out
}

// parseVersion 从工具版本串 mcp:<server>@<ver>#<hash> 里取 server 版本。
func parseVersion(spec string) string {
	rest, ok := strings.CutPrefix(spec, "mcp:")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
