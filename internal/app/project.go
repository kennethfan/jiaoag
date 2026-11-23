package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kennethfan/jiaoag/internal/config"
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
	// Summary 是「提示 2 个、技能 3 个」这样的一行描述。
	Summary string
	Prompts []string
	Skills  []string
	Servers []string
	// Err 记录发现过程中的问题（只影响出问题的那一项）。
	Err string
}

// Status 报告工作区的项目级扩展状态。mode 来自运行时配置；未配置按 ask 处理。
func (a *App) ProjectStatus(ctx context.Context, workspace, mode string) (ProjectStatus, error) {
	if mode == "" {
		mode = config.ProjectAsk
	}
	status := ProjectStatus{Mode: mode}
	workspace = canonicalRoot(workspace)
	if mode == config.ProjectOff {
		return status, nil
	}
	p, err := project.Discover(workspace)
	if err != nil {
		status.Err = err.Error()
	}
	if p.Empty() {
		return status, nil
	}
	status.Detected = true
	status.Digest = p.Digest
	status.Summary = p.Summary()
	status.Prompts = p.PromptPaths()
	status.Skills = p.SkillNames()
	status.Servers = p.ServerNames()
	if mode == config.ProjectAuto {
		status.Trusted = true
		status.Loaded = true
		return status, nil
	}
	stored, terr := a.store.ProjectTrust(ctx, workspace)
	if terr != nil {
		return status, fmt.Errorf("app: 读取项目信任记录: %w", terr)
	}
	status.Trusted = stored != "" && stored == p.Digest
	status.Loaded = status.Trusted
	return status, nil
}

// TrustProject 确认工作区当前的项目级扩展。
//
// 确认的是「当前内容摘要」而不是一个开关：仓库后续改动会得到新的摘要，
// 需要重新确认。没有可确认的内容时返回错误，避免写一条空信任。
func (a *App) TrustProject(ctx context.Context, workspace, mode string) (ProjectStatus, error) {
	workspace = canonicalRoot(workspace)
	p, err := project.Discover(workspace)
	if err != nil && p.Empty() {
		return ProjectStatus{}, fmt.Errorf("app: 读取项目级扩展: %w", err)
	}
	if p.Empty() {
		return ProjectStatus{}, fmt.Errorf("app: 工作区 %s 没有项目级扩展（AGENTS.md、.jiaoag/、.agents/skills）", workspace)
	}
	if err := a.store.TrustProject(ctx, workspace, p.Digest); err != nil {
		return ProjectStatus{}, err
	}
	return a.ProjectStatus(ctx, workspace, mode)
}

// RevokeProjectTrust 撤销工作区的项目级扩展信任。
func (a *App) RevokeProjectTrust(ctx context.Context, workspace string) error {
	return a.store.RevokeProjectTrust(ctx, canonicalRoot(workspace))
}

// loadProject 按模式与信任记录决定本次任务是否加载项目级扩展。
//
// 返回 nil 表示不加载（没有资产、被关掉或未确认）。发现过程中的错误只记
// 日志：项目扩展是增强，不是任务能否运行的前提。
func (a *App) loadProject(root string, deps *RuntimeDeps) *project.Project {
	root = canonicalRoot(root)
	mode := deps.ProjectMode
	if mode == "" {
		mode = config.ProjectAsk
	}
	if mode == config.ProjectOff {
		return nil
	}
	p, err := project.Discover(root)
	if err != nil {
		deps.Logger.Warn("读取项目级扩展时出错，已跳过出错的部分", "workspace", root, "error", err)
	}
	if p.Empty() {
		return nil
	}
	if mode == config.ProjectAuto {
		deps.Logger.Debug("已加载项目级扩展", "workspace", root, "summary", p.Summary())
		return p
	}
	stored, terr := a.store.ProjectTrust(context.Background(), root)
	if terr != nil {
		deps.Logger.Warn("读取项目信任记录失败，本次不加载项目级扩展", "workspace", root, "error", terr)
		return nil
	}
	if stored != p.Digest {
		if stored == "" {
			deps.Logger.Info("发现未确认的项目级扩展，已跳过（用 /trust 确认）", "workspace", root, "summary", p.Summary())
		} else {
			deps.Logger.Info("项目级扩展的内容已变化，需要重新确认，本次已跳过", "workspace", root, "summary", p.Summary())
		}
		return nil
	}
	deps.Logger.Debug("已加载项目级扩展", "workspace", root, "summary", p.Summary())
	return p
}

// projectLayers 把项目提示转成提示层。来源带相对路径，便于审计回答「这段谁给的」。
func projectLayers(p *project.Project) []prompt.Layer {
	if p == nil {
		return nil
	}
	out := make([]prompt.Layer, 0, len(p.Prompts))
	for _, item := range p.Prompts {
		out = append(out, prompt.Layer{Source: "project:" + item.Path, Text: item.Text})
	}
	return out
}

// projectServers 返回项目声明的 MCP server。p 为 nil 表示本次不加载项目扩展。
func projectServers(p *project.Project) []project.Server {
	if p == nil {
		return nil
	}
	return p.Servers
}

// joinProjectSkillDirs 把项目技能目录追加在机器级之后：skills.Load 是
// 「后出现者赢」，越近的仓库越具体，同名技能应由项目覆盖。
func joinProjectSkillDirs(machine []string, p *project.Project) []string {
	if p == nil || len(p.SkillDirs) == 0 {
		return machine
	}
	out := make([]string, 0, len(machine)+len(p.SkillDirs))
	out = append(out, machine...)
	out = append(out, p.SkillDirs...)
	return out
}
