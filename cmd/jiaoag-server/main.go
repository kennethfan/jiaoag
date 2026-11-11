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

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/server"
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
			fatal(err)
		}
		fmt.Printf("已写入样例配置：%s\n请填写 token 与 provider 密钥后重新启动。\n", path)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if err := cfg.ValidateServer(); err != nil {
		fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := server.New(cfg, log, version.Version)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.ListenAndServe(ctx); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag-server: %v\n", err)
	os.Exit(1)
}
