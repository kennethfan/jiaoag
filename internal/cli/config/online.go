package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"time"

	configpkg "github.com/kennethfan/jiaoag/internal/config"
	llmpkg "github.com/kennethfan/jiaoag/internal/llm"
	mcppkg "github.com/kennethfan/jiaoag/internal/mcp"
)

// runOnline 做需要出网或起子进程的探测：供应商 /models、TLS 握手、MCP 启动。
// 返回失败条数（warning 不算）。
func runOnline(cfg *configpkg.Config, filter string, stdout, stderr io.Writer) int {
	fails := 0
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	providers := cfg.Providers
	if filter != "" {
		pc, ok := cfg.Provider(filter)
		if !ok {
			fmt.Fprintf(stdout, "✗ 配置里没有供应商 %q\n", filter)
			return 1
		}
		providers = []configpkg.Provider{pc}
	}
	if len(providers) == 0 {
		fmt.Fprintln(stdout, "✗ 没有可探测的供应商")
		fails++
	}
	for _, pc := range providers {
		provider, err := llmpkg.New(pc)
		if err != nil {
			fmt.Fprintf(stdout, "✗ %s: %v\n", pc.Name, err)
			fails++
			continue
		}
		lister, ok := provider.(llmpkg.ModelLister)
		if !ok {
			fmt.Fprintf(stdout, "✗ %s: 适配器不支持模型列表\n", pc.Name)
			fails++
			continue
		}
		cctx, ccancel := context.WithTimeout(ctx, 25*time.Second)
		models, err := lister.Models(cctx)
		ccancel()
		if err != nil {
			fmt.Fprintf(stdout, "✗ %s: %v\n", pc.Name, err)
			fails++
			continue
		}
		fmt.Fprintf(stdout, "✓ %s: %d 个模型\n", pc.Name, len(models))
	}

	if cfg.Server.TLSEnabled() {
		if err := probeTLS(cfg.Server.Listen); err != nil {
			fmt.Fprintf(stdout, "✗ TLS 握手失败：%v\n", err)
			fails++
		} else {
			fmt.Fprintln(stdout, "✓ TLS 握手正常")
		}
	}

	if servers := mcpServers(cfg); len(servers) > 0 {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		mgr := mcppkg.NewManager(mcppkg.Options{Servers: servers, Logger: logger})
		defer mgr.Close()
		cwd, _ := os.Getwd()
		cctx, ccancel := context.WithTimeout(ctx, 60*time.Second)
		tools := mgr.Tools(cctx, cwd)
		ccancel()
		if len(tools) == 0 {
			fmt.Fprintf(stdout, "✗ MCP: %d 台 server 没有返回任何工具（启动失败时只会跳过，检查命令与目录）\n", len(servers))
			fails++
		} else {
			fmt.Fprintf(stdout, "✓ MCP: %d 台 server，%d 个工具\n", len(servers), len(tools))
		}
	}
	return fails
}

func probeTLS(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q 无法解析: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port),
		&tls.Config{InsecureSkipVerify: true}) // 只验证「能完成握手」，不代替客户端信任决定
	if err != nil {
		return err
	}
	defer conn.Close()
	return nil
}

// mcpServers 与 `jiaoag serve` 的转换保持一致；这里复制一份而不是导出，
// 是因为 serve 的版本还要带插件与服务端装配，config check 只需要最小的形态。
func mcpServers(cfg *configpkg.Config) []mcppkg.Server {
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
			if expanded, err := configpkg.ExpandHome(dir); err == nil {
				dir = expanded
			}
		}
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		env := make([]string, 0, len(keys))
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
