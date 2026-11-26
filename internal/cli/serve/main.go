// Command jiaoag serve 是 jiaoag 的服务端：持有会话、任务与工具执行。
package serve

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kennethfan/jiaoag/internal/boot"
	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/server"
	"github.com/kennethfan/jiaoag/internal/version"
)

func Main() {
	var (
		cfgPath     = flag.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
		listen      = flag.String("listen", "", "覆盖配置里的 server.listen")
		initConfig  = flag.Bool("init", false, "把样例配置写到默认路径后退出")
		showVersion = flag.Bool("version", false, "打印版本后退出")
		idleExit    = flag.Duration("exit-when-idle", 0, "最后一个连接断开且无排队/执行中任务后退出（0 表示常驻）")
	)
	flag.Usage = func() { helppkg.Write(os.Stdout, serveHelp(flag.CommandLine)) }
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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 装配与 TUI 内嵌模式共用 boot.Open：两条路径必须用同一套规则、后端与工具源。
	stack, err := boot.Open(ctx, boot.Options{CfgPath: cfgPath, Listen: listen, Log: log})
	if err != nil {
		return err
	}
	defer stack.Close()
	cfg := stack.Cfg

	srv, err := server.New(cfg, log, version.Version)
	if err != nil {
		return err
	}
	srv.WithApp(stack.App).SetIdleExit(idleExit)
	if cfg.Server.TLSEnabled() {
		srv.SetTLS(cfg.Server.TLSCert, cfg.Server.TLSKey)
	}
	logTokenNames(log, srv)
	go watchSIGHUP(ctx, cfgPath, srv, log)

	return srv.ListenAndServe(ctx)
}

// logTokenNames 在启动时把令牌清单打出来（只打名字与读写属性）。
//
// 这一步是排障入口：连不上时先看服务端认哪几个名字，再看客户端用的是哪一个。
func logTokenNames(log *slog.Logger, srv *server.Server) {
	tokens := srv.TokenNames()
	parts := make([]string, 0, len(tokens))
	readOnly := 0
	for _, t := range tokens {
		label := t.Name
		if t.ReadOnly {
			label += "(只读)"
			readOnly++
		}
		parts = append(parts, label)
	}
	log.Info("访问令牌已加载", "count", len(tokens), "read_only", readOnly,
		"names", strings.Join(parts, ","))
}

// watchSIGHUP 收到 SIGHUP 就重读配置文件里的令牌，整体替换。
//
// 只重载令牌：监听地址、TLS 证书、并发上限这些都要重启才生效，混在一起会让
// 「发个信号」变成不可预期的大动作。重载失败保留旧令牌，否则一次手滑的配置
// 会把所有人挡在门外。
func watchSIGHUP(ctx context.Context, cfgPath string, srv *server.Server, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			cfg, err := config.Load(cfgPath)
			if err == nil {
				var tokens []server.Token
				tokens, err = server.TokensFromConfig(cfg)
				if err == nil {
					err = srv.SetTokens(tokens)
				}
			}
			if err != nil {
				log.Error("令牌重载失败，继续使用旧令牌", "err", err)
				continue
			}
			log.Info("令牌已重载", "count", len(srv.TokenNames()))
		}
	}
}

func exit(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag serve: %v\n", err)
	os.Exit(1)
}

// serveHelp 是本子命令的 -h 输出：flag 包默认只会打「Usage of …」加一串默认值，
// 看不出这个命令在干什么。
func serveHelp(fs *flag.FlagSet) helppkg.Spec {
	return helppkg.Spec{
		Summary: "jiaoag serve 运行服务端：直接操作工作区文件，推送事件、等待审批、写审计。",
		Usage:   []string{"jiaoag serve [-config 文件] [-listen 地址] [-exit-when-idle 30s]"},
		Flags:   fs,
		Examples: []string{
			"jiaoag serve",
			"jiaoag serve -listen 127.0.0.1:9000 -exit-when-idle 30s",
			"jiaoag serve -init          # 把样例配置写到默认路径",
		},
	}
}
