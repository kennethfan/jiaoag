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

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/server"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/version"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	application := app.New(db, event.NewHub())
	defer application.Close()
	log.Info("数据已就绪", "db", cfg.Storage.DBPath)

	srv, err := server.New(cfg, log, version.Version)
	if err != nil {
		return err
	}
	srv.WithApp(application)

	return srv.ListenAndServe(ctx)
}

func exit(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag-server: %v\n", err)
	os.Exit(1)
}
