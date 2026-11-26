// Command jiaoag tui 是 jiaoag 的终端客户端：只负责渲染与审批。
package tui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/boot"
	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	launchpkg "github.com/kennethfan/jiaoag/internal/cli/launch"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
	tuipkg "github.com/kennethfan/jiaoag/internal/tui"
	"github.com/kennethfan/jiaoag/internal/version"
)

func Main() {
	var (
		cfgPath  = flag.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
		url      = flag.String("url", "", "服务端 WS 地址（默认由 server.listen 推导）")
		check    = flag.Bool("check", false, "只做一次连接与心跳验证后退出（用于自检/脚本）")
		sesID    string
		noSpawn  = flag.Bool("no-spawn", false, "不内嵌运行时、也不拉起服务端：只连已有的 jiaoag serve")
		srvBin   = flag.String("server-bin", "", "jiaoag serve 路径（默认同目录或 PATH）")
		insecure = flag.Bool("insecure", false, "跳过 TLS 证书校验（仅用于自签证书）")
		inline   = flag.Bool("inline", false, "不用全屏：保留终端 scrollback，状态栏跟在内容末尾")
		noMouse  = flag.Bool("no-mouse", false, "关闭鼠标滚轮（默认开启；开启时会占用终端原生选中，复制需按住 shift）")
	)
	flag.StringVar(&sesID, "s", "", "恢复指定会话（进程重启后用 -s <id> 回到原会话）")
	flag.StringVar(&sesID, "session", "", "同 -s（保留旧写法）")
	var rootFlags rootList
	flag.Var(&rootFlags, "root", "只读附加根，形如 alias=path；可重复（ADR-0036）")
	flag.Usage = func() { helppkg.Write(os.Stdout, tuiHelp(flag.CommandLine)) }
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
	}

	if *check {
		runCheck(opts)
		return
	}

	// 本机且没指定 -url：TUI 直接持有运行时，不经过端口、令牌与 WebSocket。
	// 但配置的地址上已经有人在监听时，一律当客户端——那可能是新版本的 serve
	// （它持有运行时锁），也可能是升级前就在跑的旧进程（它不持有锁）。后者
	// 如果我们照常内嵌，两个运行时就会同时写同一个库。
	var stack *boot.Stack
	if *url == "" && !*noSpawn && !serverReachable(target) {
		st, err := boot.Open(context.Background(), boot.Options{
			CfgPath: *cfgPath,
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		switch {
		case err == nil:
			stack = st
			defer stack.Close()
			opts.Local = func() (tuipkg.Transport, protocol.HelloPayload, error) {
				return tuipkg.NewLocalTransport(stack.Core, stack.Streams, rpc.Owner()), protocol.HelloPayload{
					ServerVersion:   version.Version,
					ProtocolVersion: protocol.Version,
					StartedAt:       time.Now().UnixMilli(),
					Listen:          "embedded",
				}, nil
			}
		case errors.Is(err, boot.ErrLocked):
			// 锁被占着但端口没人监听：说明占用者是一个内嵌 TUI，
			// 它不提供端口，第二个客户端没有入口。
			fatal(fmt.Errorf("已有一个 jiaoag 在运行并占用数据目录，但它没有监听 %s。"+
				"\n先退出那个实例，或用 -url 指定要连接的服务端", target))
		default:
			fatal(err)
		}
	}

	// 没有内嵌运行时（-url / -no-spawn / 数据目录被占用）时，本地没有 serve
	// 才拉起一个；退出后它按空闲时间自行退出。
	if stack == nil && !*noSpawn {
		cleanup, err := launchpkg.Ensure(context.Background(), cfg, *cfgPath, *srvBin, target)
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
	defer conn.Close()

	ping, err := protocol.New(protocol.TypePing, nil)
	if err != nil {
		fatal(err)
	}
	if err := conn.Send(ping); err != nil {
		fatal(fmt.Errorf("发送 ping 失败: %w", err))
	}

	env, err := conn.Read()
	if err != nil {
		fatal(fmt.Errorf("读取回帧失败: %w", err))
	}
	if env.Type != protocol.TypePong {
		var p protocol.ErrorPayload
		_ = env.Decode(&p)
		fatal(fmt.Errorf("期望 pong，收到 %s %s", env.Type, p.Message))
	}
	fmt.Printf("ok: 服务端 %s，协议 v%d，双向链路正常\n", hello.ServerVersion, hello.ProtocolVersion)
}

// serverReachable 只看端口是否有人接：错了协议、错了应用都不影响这个判断，
// 它只用来区分「别的实例在跑 serve」与「别的实例是内嵌 TUI」。
func serverReachable(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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

// tuiHelp 说明界面入口与「连不上就自己起服务端」的行为。
func tuiHelp(fs *flag.FlagSet) helppkg.Spec {
	return helppkg.Spec{
		Summary: "jiaoag tui 打开终端界面。默认在本进程内直接持有运行时（不开端口、不需要令牌）；" +
			"数据目录已被 jiaoag serve 占用时自动降级为 WS 客户端。",
		Usage: []string{"jiaoag tui [-s <会话 id>] [其他标志]"},
		Flags: fs,
		Examples: []string{
			"jiaoag tui                  # 内嵌运行时；已有一条 serve 时连它",
			"jiaoag tui -s ses_3f2a19c4  # 恢复指定会话",
			"jiaoag tui -no-spawn        # 只当客户端，不内嵌也不拉起",
			"jiaoag tui -url ws://host:8788/ws   # 连远程服务端",
		},
	}
}
