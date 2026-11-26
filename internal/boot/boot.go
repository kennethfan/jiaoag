// Package boot 负责把一个配置目录变成可用的运行时：数据库、事件、规则、
// 执行后端、工具来源与会话服务。
//
// 它同时被 `jiaoag serve`（监听端口给 Web/远程用）和 `jiaoag`（TUI 内嵌运行
// 时，直接持有同一个 Stack，不经过网络）使用。两者共用同一套装配顺序，避免
// 「直连能跑、起服务就不行」这类只在一条路径上复现的问题。
package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	notifypkg "github.com/kennethfan/jiaoag/internal/notify"
	"github.com/kennethfan/jiaoag/internal/perm"
	pluginpkg "github.com/kennethfan/jiaoag/internal/plugin"
	"github.com/kennethfan/jiaoag/internal/rpc"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/version"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// Options 描述一次启动。
type Options struct {
	// CfgPath 为空时使用默认路径。
	CfgPath string
	// Listen 非空时覆盖配置里的 server.listen（只有 serve 需要）。
	Listen string
	Log    *slog.Logger
	// Kind 是运行时类型（serve / tui），只用于观测，不影响调度。
	Kind string
}

// Stack 是一次启动的全部资源。Close 幂等，按依赖逆序收尾。
type Stack struct {
	Cfg     *config.Config
	App     *app.App
	Core    *rpc.Core
	Streams *rpc.Streams
	Procs   *execpkg.ProcTable

	db        *store.Store
	runtimeID string
	stopBeat  chan struct{}
	stopProcs context.CancelFunc
	notify    *notifypkg.Dispatcher
	mcp       *mcppkg.Manager
	plugin    *pluginpkg.Manager
	closed    bool
}

// RuntimeID 是本进程的运行时标识，写进任务占位与 runtimes 表。
func (s *Stack) RuntimeID() string { return s.runtimeID }

// DB 暴露存储句柄，供测试与只读用途核对运行时/占位状态。
func (s *Stack) DB() *store.Store { return s.db }

// Close 关闭运行时。顺序与启动相反：旁路能力先关，最后才是数据库。
func (s *Stack) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.stopProcs != nil {
		s.stopProcs()
	}
	if s.notify != nil {
		s.notify.Close()
	}
	if s.plugin != nil {
		s.plugin.Close()
	}
	if s.mcp != nil {
		s.mcp.Close()
	}
	if s.stopBeat != nil {
		close(s.stopBeat)
	}
	if s.db != nil && s.runtimeID != "" {
		// 正常退出：立刻注销，别让别的运行时等到心跳超时。
		// 必须早于 App.Close：App.Close 会关掉数据库。
		_ = s.db.RemoveRuntime(context.Background(), s.runtimeID)
	}
	if s.App != nil {
		s.App.Close()
	}
}

// Open 装配运行时。任何一步失败都会把已经拿到的资源收回去。
func Open(ctx context.Context, opts Options) (*Stack, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	cfg, err := config.Load(opts.CfgPath)
	if err != nil {
		return nil, err
	}
	if opts.Listen != "" {
		cfg.Server.Listen = opts.Listen
	}
	if err := cfg.ValidateServer(); err != nil {
		return nil, err
	}

	stack := &Stack{Cfg: cfg}
	fail := func(err error) (*Stack, error) {
		stack.Close()
		return nil, err
	}

	policyDir, err := config.ExpandHome(cfg.Policy.Dir)
	if err != nil {
		return fail(err)
	}
	feedKeys, err := cfg.FeedKeys()
	if err != nil {
		return fail(err)
	}
	ruleset, err := perm.NewLoader(policyDir).WithFeedKeys(feedKeys).Load()
	if err != nil {
		return fail(err)
	}
	for _, w := range ruleset.Warnings {
		log.Warn("规则加载提示", "detail", w)
	}
	backend, err := newExecBackend(cfg.Exec.Backend)
	if err != nil {
		return fail(err)
	}

	db, err := store.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return fail(err)
	}
	stack.db = db
	// 运行时注册与心跳：多进程共用一个数据目录时，靠它判断谁还活着。
	stack.runtimeID = newRuntimeID()
	kind := opts.Kind
	if kind == "" {
		kind = "tui"
	}
	now := time.Now()
	if err := db.RegisterRuntime(ctx, store.Runtime{
		ID: stack.runtimeID, PID: os.Getpid(), Host: hostname(),
		Kind: kind, StartedAt: now, HeartbeatAt: now,
	}); err != nil {
		return fail(err)
	}
	stack.stopBeat = make(chan struct{})
	go heartbeatLoop(db, stack.runtimeID, stack.stopBeat, log)

	application := app.New(db, event.NewHub())
	stack.App = application

	stack.mcp = mcppkg.NewManager(mcppkg.Options{Servers: mcpServers(cfg), Logger: log})
	stack.plugin = pluginpkg.NewManager(pluginpkg.Options{
		Servers: pluginServers(cfg),
		Logger:  log,
		Version: version.Version,
	})
	stack.notify = notifypkg.New(notifyHooks(cfg, log), log)

	// 后台命令表按单会话上限 4 条（超出由工具层报错，不排队）。
	// 接上共享存储：别的运行时能看到这些进程，并只能投递停止意图。
	stack.Procs = execpkg.NewProcTable(4)
	procCtx, stopProcs := context.WithCancel(context.Background())
	stack.stopProcs = stopProcs
	stack.Procs.Attach(procStore{db: db}, stack.runtimeID, procCtx)
	// 属主已经失联的后台进程只标 orphaned，不代为 kill（pid 可能被复用）。
	if lives, err := db.LiveRuntimes(ctx, time.Now().Add(-30*time.Second)); err == nil {
		ids := make([]string, 0, len(lives))
		for _, rt := range lives {
			ids = append(ids, rt.ID)
		}
		if n, err := db.OrphanBackgroundProcs(ctx, ids); err != nil {
			log.Warn("标记孤儿后台进程失败", "err", err)
		} else if n > 0 {
			log.Warn("发现属主已失联的后台进程，只读展示不再管理", "count", n)
		}
	}
	deps := runtimeDeps(cfg, log, backend, ruleset, stack.mcp, stack.plugin, stack.Procs)
	deps.RuntimeID = stack.runtimeID
	if stack.notify != nil {
		deps.Notify = stack.notify.Observe
	}
	if err := application.ConfigureRuntime(deps); err != nil {
		return fail(err)
	}
	log.Info("数据已就绪", "db", cfg.Storage.DBPath)
	log.Info("执行环境已就绪", "policy", ruleset.Version, "policy_hash", ruleset.Hash[:12],
		"rules", len(ruleset.Rules), "policy_dir", policyDir, "backend", backend.Name())

	stack.Core = rpc.NewCore(application, cfg, log)
	stack.Streams = rpc.NewStreams(application, log)
	return stack, nil
}

// newRuntimeID 生成运行时标识：主机 + pid + 启动时刻，足以区分同机多进程，
// 也能在看日志时一眼认出是谁。
func newRuntimeID() string {
	return fmt.Sprintf("%s:%d:%d", hostname(), os.Getpid(), time.Now().UnixNano())
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// heartbeatLoop 每 5 秒刷新一次心跳。进程崩溃时心跳停止，30 秒后被视为死亡。
func heartbeatLoop(db *store.Store, id string, stop <-chan struct{}, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := db.HeartbeatRuntime(context.Background(), id, time.Now()); err != nil {
				log.Warn("刷新运行时心跳失败", "err", err)
			}
		}
	}
}

// notifyHooks 把配置翻译成通知端点。secret 解析失败只跳过该端点：
// 通知是旁路能力，不该因为它配错而让服务端起不来（配置校验已先挡过一遍）。
func notifyHooks(cfg *config.Config, log *slog.Logger) []notifypkg.Hook {
	var hooks []notifypkg.Hook
	for _, w := range cfg.Notify.Webhooks {
		if !w.IsEnabled() {
			continue
		}
		secret := ""
		if w.Secret != "" {
			v, err := w.Secret.Resolve()
			if err != nil {
				log.Error("通知 secret 解析失败，跳过该端点", "hook", w.Name, "err", err)
				continue
			}
			secret = v
		}
		events := make(map[notifypkg.Kind]bool, len(w.Events))
		for _, e := range w.Events {
			events[notifypkg.Kind(strings.TrimSpace(e))] = true
		}
		hooks = append(hooks, notifypkg.Hook{
			Name:    w.Name,
			URL:     w.URL,
			Secret:  secret,
			Events:  events,
			Timeout: time.Duration(w.TimeoutSeconds) * time.Second,
		})
	}
	return hooks
}

// newExecBackend 按配置选执行后端。sandbox 不可用时直接报错：降级必须
// 显式（ADR-0011），不能悄悄回退到 direct。
func newExecBackend(name string) (execpkg.Backend, error) {
	switch name {
	case config.DefaultExecBackend:
		return execpkg.NewDirect(), nil
	case config.SandboxExecBackend:
		b, err := execpkg.NewSandbox()
		if err != nil {
			return nil, fmt.Errorf("exec.backend=sandbox 不可用: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("未知执行后端 %q", name)
	}
}

// runtimeDeps 把配置翻译成执行依赖：app 不认识 config，这里做唯一的接线。
func runtimeDeps(cfg *config.Config, log *slog.Logger, backend execpkg.Backend, ruleset *perm.Ruleset, mcpManager *mcppkg.Manager, pluginManager *pluginpkg.Manager, procs *execpkg.ProcTable) app.RuntimeDeps {
	outputDir, err := config.ExpandHome(cfg.Storage.OutputDir)
	if err != nil {
		outputDir = cfg.Storage.OutputDir
	}
	return app.RuntimeDeps{
		Procs: procs,
		Provider: func(sess domain.Session) (llm.Provider, string, error) {
			provider, ok := cfg.Provider(sess.Provider)
			if sess.Provider == "" {
				provider, ok = cfg.DefaultProvider()
			}
			if !ok {
				return nil, "", fmt.Errorf("配置里没有供应商 %q", sess.Provider)
			}
			built, err := llm.New(provider)
			if err != nil {
				return nil, "", err
			}
			model := sess.Model
			if model == "" {
				model = provider.Model
			}
			return built, model, nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			env.EventLimit = cfg.Exec.EventLimit
			env.LogLimit = cfg.Exec.LogLimit
			base := []tools.Tool{
				tools.NewReadFileWithRoots(ws, env.Roots),
				tools.NewGrepWithRoots(ws, env.Roots),
				tools.NewGlobWithRoots(ws, env.Roots),
				tools.NewWriteFileWithRoots(ws, env.Roots), tools.NewEditFileWithRoots(ws, env.Roots),
				tools.NewRunCommand(ws, env), tools.NewCheckCommand(ws, env),
				tools.NewReadOutput(env), tools.NewStopCommand(env),
				tools.NewGitStatus(ws, env), tools.NewGitDiff(ws, env), tools.NewGitLog(ws, env),
			}
			if cfg.Agent.MemoryEnabled() {
				base = append(base, tools.NewRemember(ws))
			}
			// 技能索引与工具同源：catalog 由 app 按工作区加载后经 env 传进来。
			if env.Skills != nil {
				base = append(base, tools.NewSkill(env.Skills))
			}
			// MCP 是增强不是前提：server 起不来时该来源给出空列表，内置工具照常。
			// 项目级 server 与全局同名时覆盖全局（mergeServers 统一处理）。
			sources := []tools.Source{
				tools.NewStaticSource("builtin", base...),
				mcpManager.Source(ws.Root(), mcppkg.ServersFromProject(env.ProjectServers)),
				// 插件同样只是增强：进程起不来时该来源为空，其余工具照常。
				pluginManager.Source(ws, env),
			}
			registry, errs := tools.NewRegistryFrom(context.Background(), sources...)
			for _, err := range errs {
				log.Warn("工具来源部分不可用", "workspace", ws.Root(), "err", err)
			}
			return registry, nil
		},
		MCPServers: func(root string, extra []mcppkg.Server) []mcppkg.ServerStatus {
			return mcpManager.Status(root, extra)
		},
		Backend:     backend,
		Ruleset:     ruleset,
		OutputDir:   outputDir,
		ExecTimeout: time.Duration(cfg.Exec.TimeoutSeconds) * time.Second,
		Limits: execpkg.Limits{
			CPUSeconds: cfg.Exec.Limits.CPUSeconds,
			MemoryMB:   cfg.Exec.Limits.MemoryMB,
			MaxFiles:   cfg.Exec.Limits.MaxFiles,
		},
		MaxConcurrentTasks: cfg.Agent.MaxConcurrentTasks,
		MaxQueueDepth:      cfg.Agent.MaxQueueDepth,
		MaxRounds:          agentMaxRounds(cfg.Agent.MaxRoundsValue()),
		ContextBudget:      cfg.Agent.ContextBudgetTokenValue(),
		ContextCompact:     cfg.Agent.ContextCompact == config.ContextCompactAuto,
		ResumeQueued:       cfg.Agent.ResumeQueuedEnabled(),
		ApprovalTTL:        cfg.Agent.ApprovalTTLDuration(),
		WriteApproval:      cfg.Agent.WriteApprovalMode(),
		Memory:             cfg.Agent.MemoryEnabled(),
		SkillDirs:          cfg.Agent.SkillDirsFor(),
		ProjectMode:        cfg.Project.Mode,
		Logger:             log,
	}
}

// agentMaxRounds 把配置的轮数翻译成 agent 的约定：0 表示不限制，用 -1 表示。
func agentMaxRounds(rounds int) int {
	if rounds == 0 {
		return -1
	}
	return rounds
}

// mcpServers 把配置翻译成 Manager 的启动描述。禁用的条目直接不出现：
// 停用不该是「启动后再关」，那会白起一个进程。
func mcpServers(cfg *config.Config) []mcppkg.Server {
	out := make([]mcppkg.Server, 0, len(cfg.MCP.Servers))
	for _, s := range cfg.MCP.Servers {
		if !s.IsEnabled() {
			continue
		}
		argv := make([]string, 0, 1+len(s.Args))
		argv = append(argv, s.Command)
		argv = append(argv, s.Args...)
		dir := s.Dir
		if dir != "" {
			if expanded, err := config.ExpandHome(dir); err == nil {
				dir = expanded
			}
		}
		env := make([]string, 0, len(s.Env))
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+s.Env[k])
		}
		include, exclude := s.ToolFilter()
		out = append(out, mcppkg.Server{
			Name:    s.Name,
			Argv:    argv,
			Env:     env,
			Dir:     dir,
			Timeout: time.Duration(s.TimeoutSeconds) * time.Second,
			Trust:   s.Trust == "allow",
			Include: include,
			Exclude: exclude,
		})
	}
	return out
}

// pluginServers 把配置转成插件管理器的形态。
func pluginServers(cfg *config.Config) []pluginpkg.Server {
	out := make([]pluginpkg.Server, 0, len(cfg.Plugins.Servers))
	for _, s := range cfg.Plugins.Servers {
		if !s.IsEnabled() {
			continue
		}
		argv := make([]string, 0, 1+len(s.Args))
		argv = append(argv, s.Command)
		argv = append(argv, s.Args...)
		dir := s.Dir
		if dir != "" {
			if expanded, err := config.ExpandHome(dir); err == nil {
				dir = expanded
			}
		}
		env := make([]string, 0, len(s.Env))
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+s.Env[k])
		}
		out = append(out, pluginpkg.Server{
			Name:    s.Name,
			Argv:    argv,
			Env:     env,
			Dir:     dir,
			Timeout: time.Duration(s.TimeoutSeconds) * time.Second,
		})
	}
	return out
}

// ErrLocked 表示同一数据目录已经有运行时在跑。
var ErrLocked = errors.New("boot: 已经有一个 jiaoag 运行时在使用这个数据目录")

// procStore 把 exec 的后台进程投影写进共享库。放在 boot 里而不是 exec 内部，
// 是为了让 exec 保持「不依赖存储」的定位。
type procStore struct{ db *store.Store }

func (p procStore) RegisterProc(ctx context.Context, rec execpkg.ProcRecord) error {
	return p.db.RegisterBackgroundProc(ctx, store.BackgroundProc{
		ID:         rec.ID,
		SessionID:  rec.SessionID,
		Owner:      rec.Owner,
		PID:        rec.PID,
		OutputPath: rec.OutputPath,
		StartedAt:  rec.StartedAt,
		Status:     rec.Status,
	})
}

func (p procStore) MarkProc(ctx context.Context, id, status string) error {
	return p.db.MarkBackgroundProc(ctx, id, status)
}

func (p procStore) ListProcs(ctx context.Context, sessionID string) ([]execpkg.ProcRecord, error) {
	list, err := p.db.ListBackgroundProcs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]execpkg.ProcRecord, 0, len(list))
	for _, rec := range list {
		out = append(out, execpkg.ProcRecord{
			ID:         rec.ID,
			SessionID:  rec.SessionID,
			Owner:      rec.Owner,
			OutputPath: rec.OutputPath,
			PID:        rec.PID,
			StartedAt:  rec.StartedAt,
			Status:     rec.Status,
		})
	}
	return out, nil
}

func (p procStore) RequestStop(ctx context.Context, sessionID, id string) error {
	return p.db.RequestBackgroundProcStop(ctx, sessionID, id)
}

func (p procStore) StopRequested(ctx context.Context, id string) (bool, error) {
	return p.db.BackgroundProcStopRequested(ctx, id)
}
