// Command jiaoag-tui 是 jiaoag 的终端客户端：只负责渲染与审批。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/tui"
)

func main() {
	var (
		cfgPath = flag.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
		url     = flag.String("url", "", "服务端 WS 地址（默认由 server.listen 推导）")
		check   = flag.Bool("check", false, "只做一次连接与心跳验证后退出（用于自检/脚本）")
		sesID   = flag.String("session", "", "恢复指定会话（进程重启后用 --session <id> 回到原会话）")
	)
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if err := cfg.ValidateClient(); err != nil {
		fatal(err)
	}
	token, err := cfg.Server.Token.Resolve()
	if err != nil {
		fatal(fmt.Errorf("解析 server.token 失败: %w", err))
	}

	target := *url
	if target == "" {
		target = "ws://" + dialableAddr(cfg.Server.Listen) + "/ws"
	}
	opts := tui.Options{URL: target, Token: token, SessionID: *sesID}

	if *check {
		runCheck(opts)
		return
	}

	if _, err := tea.NewProgram(tui.New(opts)).Run(); err != nil {
		fatal(err)
	}
}

// runCheck 在没有终端的场景下验证握手与一次 ping/pong 往返。
func runCheck(opts tui.Options) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, hello, err := tui.Dial(ctx, opts)
	if err != nil {
		fatal(err)
	}
	defer conn.CloseNow()

	ping, err := protocol.New(protocol.TypePing, nil)
	if err != nil {
		fatal(err)
	}
	if err := wsjson.Write(ctx, conn, ping); err != nil {
		fatal(fmt.Errorf("发送 ping 失败: %w", err))
	}

	var env protocol.Envelope
	if err := wsjson.Read(ctx, conn, &env); err != nil {
		fatal(fmt.Errorf("读取回帧失败: %w", err))
	}
	if env.Type != protocol.TypePong {
		var p protocol.ErrorPayload
		_ = env.Decode(&p)
		fatal(fmt.Errorf("期望 pong，收到 %s %s", env.Type, p.Message))
	}
	fmt.Printf("ok: 服务端 %s，协议 v%d，双向链路正常\n", hello.ServerVersion, hello.ProtocolVersion)
}

// dialableAddr 把监听地址换成客户端可连的地址：0.0.0.0 与空主机名都落到回环。
func dialableAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return strings.TrimSpace(net.JoinHostPort(host, port))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag-tui: %v\n", err)
	os.Exit(1)
}
