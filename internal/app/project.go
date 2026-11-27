package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	"github.com/kennethfan/jiaoag/internal/memory"
	"github.com/kennethfan/jiaoag/internal/project"
	"github.com/kennethfan/jiaoag/internal/prompt"
)

// canonicalRoot 把工作区路径统一成绝对、已解析符号链接的形式。
//
// 信任记录以工作区路径为键，而 macOS 的 /var 是 /private/var 的符号链接：
// 一边按 t.TempDir() 的 /var 写、一边按 workspace.Root() 的 /private/var
// 读，同一份仓库会永远对不上。两处都走这里就没有这个问题。
func canonicalRoot(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// ProjectStatus 是某个工作区项目级扩展的当前状态，供会话创建与 /trust 展示。
type ProjectStatus struct {
	// Mode 是生效模式：ask / auto / off。
	Mode string
	// Detected 为真表示发现了项目资产（不管是否加载）。
	Detected bool
	// Trusted 为真表示当前摘要已确认（auto 模式下恒为真）。
	Trusted bool
	// Loaded 为真表示这次任务会真正使用这些资产。
	Loaded bool
	Digest string
	// Summary 是「提示 2 个、技能 3 个」这样的一行描述；多根时会注明来源。
	Summary string
	// Prompts / Skills / Servers 用带别名前缀的名字展示，附加根的条目形如
	// lib:AGENTS.md、lib:lint、lib/fs: npx …。
	Prompts []string
	Skills  []string
	Servers []string
	// Err 记录发现过程中的问题（只影响出问题的那一项）。
	Err string
}

// discoverProject 发现主根与各附加根的项目级扩展并合并。
//
// 读不了的文件只报错不中断：项目扩展是增强，不是运行前提。返回的 Multi
// 永远非 nil；Sites 里保留所有根（包括没有资产的），记忆要用它们。
func discoverProject(workspace string, roots []domain.SessionRoot) (*project.Multi, []error) {
	sites := make([]project.Site, 0, len(roots)+1)
	var errs []error
	primary, err := project.Discover(workspace)
	if err != nil {
		errs = append(errs, err)
	}
	sites = append(sites, project.Site{Project: primary})
	for _, root := range roots {
		path := canonicalRoot(root.Path)
		p, err := project.Discover(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", root.Alias, err))
		}
		sites = append(sites, project.Site{Alias: root.Alias, Project: p})
	}
	return project.Merge(sites), errs
}

// Status 报告工作区的项目级扩展状态。mode 来自运行时配置；未配置按 ask 处理。
//
// 摘要覆盖主根与全部附加根，所以「加挂一个仓库」「某个根的内容变了」都会让
// 已确认的摘要失效、要求重新 /trust——不能因为确认过主根就静默接受新增根
// 塞进来的提示（ADR-0055）。
func (a *App) ProjectStatus(ctx context.Context, workspace string, roots []domain.SessionRoot, mode string) (ProjectStatus, error) {
	if mode == "" {
		mode = config.ProjectAsk
	}
	status := ProjectStatus{Mode: mode}
	workspace = canonicalRoot(workspace)
	if mode == config.ProjectOff {
		return status, nil
	}
	merged, errs := discoverProject(workspace, roots)
	if len(errs) > 0 {
		status.Err = errors.Join(errs...).Error()
	}
	if merged.Empty() {
		return status, nil
	}
	status.Detected = true
	status.Digest = merged.Digest
	status.Summary = merged.Summary()
	status.Prompts = merged.PromptPaths()
	status.Skills = merged.SkillNames()
	status.Servers = merged.ServerNames()
	if mode == config.ProjectAuto {
		status.Trusted = true
		status.Loaded = true
		return status, nil
	}
	stored, terr := a.store.ProjectTrust(ctx, workspace)
	if terr != nil {
		return status, fmt.Errorf("app: 读取项目信任记录: %w", terr)
	}
	status.Trusted = stored != "" && stored == merged.Digest
	status.Loaded = status.Trusted
	return status, nil
}

// TrustProject 确认工作区当前的项目级扩展（主根 + 全部附加根）。
//
// 确认的是「当前内容摘要」而不是一个开关：仓库后续改动、增删附加根都会得到
// 新的摘要，需要重新确认。没有可确认的内容时返回错误，避免写一条空信任。
func (a *App) TrustProject(ctx context.Context, workspace string, roots []domain.SessionRoot, mode string) (ProjectStatus, error) {
	workspace = canonicalRoot(workspace)
	merged, errs := discoverProject(workspace, roots)
	if merged.Empty() {
		if len(errs) > 0 {
			return ProjectStatus{}, fmt.Errorf("app: 读取项目级扩展: %w", errors.Join(errs...))
		}
		return ProjectStatus{}, fmt.Errorf("app: 工作区 %s 没有项目级扩展（AGENTS.md、.jiaoag/、.agents/skills）", workspace)
	}
	if err := a.store.TrustProject(ctx, workspace, merged.Digest); err != nil {
		return ProjectStatus{}, err
	}
	return a.ProjectStatus(ctx, workspace, roots, mode)
}

// RevokeProjectTrust 撤销工作区的项目级扩展信任。信任按主根存一条，撤销即
// 同时撤销全部附加根——它们是同一次确认的一部分。
func (a *App) RevokeProjectTrust(ctx context.Context, workspace string) error {
	return a.store.RevokeProjectTrust(ctx, canonicalRoot(workspace))
}

// loadProject 按模式与信任记录决定本次任务是否加载项目级扩展。
//
// 返回 nil 表示不加载（没有资产、被关掉或未确认）。发现过程中的错误只记
// 日志：项目扩展是增强，不是任务能否运行的前提。
func (a *App) loadProject(workspace string, roots []domain.SessionRoot, deps *RuntimeDeps) *project.Multi {
	workspace = canonicalRoot(workspace)
	mode := deps.ProjectMode
	if mode == "" {
		mode = config.ProjectAsk
	}
	if mode == config.ProjectOff {
		return nil
	}
	merged, errs := discoverProject(workspace, roots)
	for _, err := range errs {
		deps.Logger.Warn("读取项目级扩展时出错，已跳过出错的部分", "workspace", workspace, "error", err)
	}
	if merged.Empty() {
		return nil
	}
	if mode == config.ProjectAuto {
		deps.Logger.Debug("已加载项目级扩展", "workspace", workspace, "summary", merged.Summary())
		return merged
	}
	stored, terr := a.store.ProjectTrust(context.Background(), workspace)
	if terr != nil {
		deps.Logger.Warn("读取项目信任记录失败，本次不加载项目级扩展", "workspace", workspace, "error", terr)
		return nil
	}
	if stored != merged.Digest {
		if stored == "" {
			deps.Logger.Info("发现未确认的项目级扩展，已跳过（用 /trust 确认）", "workspace", workspace, "summary", merged.Summary())
		} else {
			deps.Logger.Info("项目级扩展的内容已变化，需要重新确认，本次已跳过", "workspace", workspace, "summary", merged.Summary())
		}
		return nil
	}
	deps.Logger.Debug("已加载项目级扩展", "workspace", workspace, "summary", merged.Summary())
	return merged
}

// projectLayers 把项目提示转成提示层。来源带别名与相对路径，便于审计回答
// 「这段谁给的」（project:lib/AGENTS.md）。
func projectLayers(m *project.Multi) []prompt.Layer {
	if m == nil {
		return nil
	}
	prompts := m.Prompts()
	out := make([]prompt.Layer, 0, len(prompts))
	for _, item := range prompts {
		out = append(out, prompt.Layer{Source: "project:" + item.Path, Text: item.Text})
	}
	return out
}

// projectServers 返回项目声明的 MCP server（已按主根优先合并）。
func projectServers(m *project.Multi) []project.Server {
	if m == nil {
		return nil
	}
	return m.Servers()
}

// MCPServers 返回该工作区会用的 MCP server 与运行状态，供 /mcp 面板展示。
//
// 只读快照：不启动进程，也不改变任何状态。未配置 MCP 时返回 nil，不是错误——
// 「没用 MCP」是正常情况，不该让面板报错。
func (a *App) MCPServers(workspace string, roots []domain.SessionRoot) []mcppkg.ServerStatus {
	deps := a.currentRuntime()
	if deps == nil || deps.MCPServers == nil {
		return nil
	}
	root := canonicalRoot(workspace)
	proj := a.loadProject(root, roots, deps)
	return deps.MCPServers(root, mcppkg.ServersFromProject(projectServers(proj)))
}

// joinProjectSkillDirs 把项目技能目录追加在机器级之后：skills.Load 是
// 「后出现者赢」，越近的仓库越具体。Multi.SkillDirs 已经把主根排在最后，
// 所以同名技能由主根胜出、附加根之间按别名顺序后者胜出。
func joinProjectSkillDirs(machine []string, m *project.Multi) []string {
	if m == nil {
		return machine
	}
	dirs := m.SkillDirs()
	if len(dirs) == 0 {
		return machine
	}
	out := make([]string, 0, len(machine)+len(dirs))
	out = append(out, machine...)
	out = append(out, dirs...)
	return out
}

// loadMemory 读取主根与附加根的记忆并拼成一层正文。
//
// 主根的记忆一直加载（与单根工作区行为一致）；附加根的记忆只在项目级扩展
// 被加载（已确认或 auto）时才有机会出现——别人的仓库里的 MEMORY.md 同样是
// 可以指挥 agent 的文本，没确认就不该进上下文。
func loadMemory(sites []project.Site, onlyPrimary bool, log *slog.Logger) string {
	entries := make([]memory.Entry, 0, len(sites))
	for _, site := range sites {
		if onlyPrimary && site.Alias != "" {
			continue
		}
		if site.Project == nil {
			continue
		}
		text, truncated, err := memory.Load(site.Project.Root)
		if err != nil {
			log.Warn("读取跨会话记忆失败，本次不注入该根", "root", site.Project.Root, "error", err)
			continue
		}
		if truncated {
			log.Warn("跨会话记忆超过上限，已截断注入", "root", site.Project.Root, "file", memory.FileName)
		}
		if text == "" {
			continue
		}
		entries = append(entries, memory.Entry{Label: site.Alias, Text: text})
	}
	return memory.Combine(entries, memory.Budget)
}
