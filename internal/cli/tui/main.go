// Command jiaoag tui 是 jiaoag 的终端客户端：只负责渲染与审批。
package tui

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
	tuipkg "github.com/kennethfan/jiaoag/internal/tui"
)

func Main() {
	var (
		cfgPath  = flag.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
		url      = flag.String("url", "", "服务端 WS 地址（默认由 server.listen 推导）")
		check    = flag.Bool("check", false, "只做一次连接与心跳验证后退出（用于自检/脚本）")
		sesID    string
		noSpawn  = flag.Bool("no-spawn", false, "连不上时也不自动拉起本地服务端")
		srvBin   = flag.String("server-bin", "", "jiaoag serve 路径（默认同目录或 PATH）")
		insecure = flag.Bool("insecure", false, "跳过 TLS 证书校验（仅用于自签证书）")
		inline   = flag.Bool("inline", false, "不用全屏：保留终端 scrollback，状态栏跟在内容末尾")
		noMouse  = flag.Bool("no-mouse", false, "关闭鼠标滚轮（默认开启；开启时会占用终端原生选中，复制需按住 shift）")
		cont     = flag.Bool("c", false, "继续本目录下最近更新的会话（没有则新建；-s 优先）")
	)
	flag.StringVar(&sesID, "s", "", "恢复指定会话（进程重启后用 -s <id> 回到原会话）")
	flag.StringVar(&sesID, "session", "", "同 -s（保留旧写法）")
	var rootFlags rootList
	flag.Var(&rootFlags, "root", "只读附加根，形如 alias=path；可重复（ADR-0036）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if err := cfg.ValidateClient(); err != nil {
		fatal(err)
	}
	secret, err := cfg.Server.ClientToken()
	if err != nil {
		fatal(err)
	}
	token, err := secret.Resolve()
	if err != nil {
		fatal(fmt.Errorf("解析 server.token 失败: %w", err))
	}

	target := *url
	if target == "" {
		scheme := "ws"
		if cfg.Server.TLSEnabled() {
			scheme = "wss"
		}
		target = scheme + "://" + dialableAddr(cfg.Server.Listen) + "/ws"
	}
	roots, err := rootFlags.payloads()
	if err != nil {
		fatal(err)
	}
	opts := tuipkg.Options{
		URL: target, Token: token, SessionID: sesID, InsecureTLS: *insecure,
		Roots: roots, Inline: *inline, Mouse: !*noMouse,
		ContinueLast: *cont && sesID == "",
	}

	if *check {
		runCheck(opts)
		return
	}

	// 本地没有服务端时自动拉起一个，退出时再收掉；远端地址与 -no-spawn 不碰进程。
	if !*noSpawn {
		cleanup, err := ensureServer(context.Background(), cfg, *cfgPath, *srvBin, target)
		if err != nil {
			fatal(err)
		}
		defer cleanup()
	}

	if _, err := tea.NewProgram(tuipkg.New(opts)).Run(); err != nil {
		fatal(err)
	}
}

// runCheck 在没有终端的场景下验证握手与一次 ping/pong 往返。
func runCheck(opts tuipkg.Options) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, hello, err := tuipkg.Dial(ctx, opts)
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
	fmt.Fprintf(os.Stderr, "jiaoag tui: %v\n", err)
	os.Exit(1)
}

// rootList 收集 -root alias=path（可重复）。
type rootList []string

func (r *rootList) String() string { return strings.Join(*r, ",") }

func (r *rootList) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func (r rootList) payloads() ([]protocol.SessionRootPayload, error) {
	out := make([]protocol.SessionRootPayload, 0, len(r))
	seen := map[string]bool{}
	for _, item := range r {
		alias, path, ok := strings.Cut(item, "=")
		alias = strings.TrimSpace(alias)
		path = strings.TrimSpace(path)
		if !ok || alias == "" || path == "" {
			return nil, fmt.Errorf("-root 需要 alias=path 形式，收到 %q", item)
		}
		if seen[alias] {
			return nil, fmt.Errorf("-root 别名 %q 重复", alias)
		}
		seen[alias] = true
		out = append(out, protocol.SessionRootPayload{Alias: alias, Path: path})
	}
	return out, nil
}
