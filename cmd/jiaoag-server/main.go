// Command jiaoag-server 是 jiaoag 的服务端：持有会话、任务与工具执行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/server"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/version"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func main() {
	var (
		cfgPath     = flag.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
		listen      = flag.String("listen", "", "覆盖配置里的 server.listen")
		initConfig  = flag.Bool("init", false, "把样例配置写到默认路径后退出")
		showVersion = flag.Bool("version", false, "打印版本后退出")
		idleExit    = flag.Duration("exit-when-idle", 0, "最后一个连接断开且无排队/执行中任务后退出（0 表示常驻）")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	if *initConfig {
		path := *cfgPath
		if path == "" {
			path = config.DefaultPath()
		}
		if err := config.WriteExample(path); err != nil {
			exit(err)
		}
		fmt.Printf("已写入样例配置：%s\n请填写 token 与 provider 密钥后重新启动。\n", path)
		return
	}

	if err := run(*cfgPath, *listen, *idleExit); err != nil {
		exit(err)
	}
}

// run 承载启动流程：所有失败都以 error 返回，不用 os.Exit 打断，
// 这样 defer 里的 application.Close() 一定会执行（WAL 正常 checkpoint）。
func run(cfgPath, listen string, idleExit time.Duration) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if listen != "" {
		cfg.Server.Listen = listen
	}
	if err := cfg.ValidateServer(); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	policyDir, err := config.ExpandHome(cfg.Policy.Dir)
	if err != nil {
		return err
	}
	ruleset, err := perm.NewLoader(policyDir).Load()
	if err != nil {
		return err
	}
	backend, err := newExecBackend(cfg.Exec.Backend)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	application := app.New(db, event.NewHub())
	defer application.Close()

	mcpManager := mcppkg.NewManager(mcppkg.Options{Servers: mcpServers(cfg), Logger: log})
	defer mcpManager.Close()
	if err := application.ConfigureRuntime(runtimeDeps(cfg, log, backend, ruleset, mcpManager)); err != nil {
		return err
	}
	log.Info("数据已就绪", "db", cfg.Storage.DBPath)
	log.Info("执行环境已就绪", "policy", ruleset.Version, "policy_hash", ruleset.Hash[:12], "backend", backend.Name())

	srv, err := server.New(cfg, log, version.Version)
	if err != nil {
		return err
	}
	srv.WithApp(application).SetIdleExit(idleExit)
	if cfg.Server.TLSEnabled() {
		srv.SetTLS(cfg.Server.TLSCert, cfg.Server.TLSKey)
	}

	return srv.ListenAndServe(ctx)
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
func runtimeDeps(cfg *config.Config, log *slog.Logger, backend execpkg.Backend, ruleset *perm.Ruleset, mcpManager *mcppkg.Manager) app.RuntimeDeps {
	outputDir, err := config.ExpandHome(cfg.Storage.OutputDir)
	if err != nil {
		outputDir = cfg.Storage.OutputDir
	}
	return app.RuntimeDeps{
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
				tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
				tools.NewWriteFile(ws), tools.NewEditFile(ws),
				tools.NewRunCommand(ws, env), tools.NewCheckCommand(ws, env),
				tools.NewReadOutput(env),
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
		ContextBudget:      cfg.Agent.ContextBudgetChars,
		ContextCompact:     cfg.Agent.ContextCompact == config.ContextCompactAuto,
		ResumeQueued:       cfg.Agent.ResumeQueuedEnabled(),
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

func exit(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag-server: %v\n", err)
	os.Exit(1)
}
