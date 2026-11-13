// Command jiaoag-server 是 jiaoag 的服务端：持有会话、任务与工具执行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/llm"
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

	if err := run(*cfgPath, *listen); err != nil {
		exit(err)
	}
}

// run 承载启动流程：所有失败都以 error 返回，不用 os.Exit 打断，
// 这样 defer 里的 application.Close() 一定会执行（WAL 正常 checkpoint）。
func run(cfgPath, listen string) error {
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
	backend := execpkg.NewDirect()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	application := app.New(db, event.NewHub())
	defer application.Close()
	if err := application.ConfigureRuntime(runtimeDeps(cfg, log, backend, ruleset)); err != nil {
		return err
	}
	log.Info("数据已就绪", "db", cfg.Storage.DBPath)
	log.Info("执行环境已就绪", "policy", ruleset.Version, "policy_hash", ruleset.Hash[:12], "backend", backend.Name())

	srv, err := server.New(cfg, log, version.Version)
	if err != nil {
		return err
	}
	srv.WithApp(application)

	return srv.ListenAndServe(ctx)
}

// runtimeDeps 把配置翻译成执行依赖：app 不认识 config，这里做唯一的接线。
func runtimeDeps(cfg *config.Config, log *slog.Logger, backend execpkg.Backend, ruleset *perm.Ruleset) app.RuntimeDeps {
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
			return tools.NewRegistry(
				tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws),
				tools.NewWriteFile(ws), tools.NewEditFile(ws),
				tools.NewRunCommand(ws, env), tools.NewCheckCommand(ws, env),
				tools.NewReadOutput(env),
				tools.NewGitStatus(ws, env), tools.NewGitDiff(ws, env), tools.NewGitLog(ws, env),
			)
		},
		Backend:       backend,
		Ruleset:       ruleset,
		OutputDir:     outputDir,
		ExecTimeout:   time.Duration(cfg.Exec.TimeoutSeconds) * time.Second,
		MaxRounds:     cfg.Agent.MaxRounds,
		ContextBudget: cfg.Agent.ContextBudgetChars,
		Logger:        log,
	}
}

func exit(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag-server: %v\n", err)
	os.Exit(1)
}
