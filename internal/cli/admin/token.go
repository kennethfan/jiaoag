package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	"github.com/kennethfan/jiaoag/internal/config"
	serverpkg "github.com/kennethfan/jiaoag/internal/server"
	"github.com/kennethfan/jiaoag/internal/store"
)

// tokenHelp 是 `jiaoag admin token -h`。
func tokenHelp(fs *flag.FlagSet) helppkg.Spec {
	return helppkg.Spec{
		Summary: "管理访问令牌：令牌以数据库为准，改完 2 秒内服务端自动生效（Windows 也一样）。",
		Usage: []string{
			"jiaoag admin token list",
			"jiaoag admin token create <名字> [-read-only]",
			"jiaoag admin token rotate <名字>",
			"jiaoag admin token revoke <名字> [-yes]",
		},
		Commands: []helppkg.Command{
			{Name: "list", Desc: "列出全部令牌（含已吊销），只显示哈希前缀"},
			{Name: "create", Desc: "新建令牌，明文只显示这一次"},
			{Name: "rotate", Desc: "换掉令牌明文，旧值立刻失效"},
			{Name: "revoke", Desc: "吊销令牌（保留记录，不再接受认证）"},
		},
		Flags: fs,
		Examples: []string{
			"jiaoag admin token create viewer -read-only   # 给导出/看板用的只读令牌",
			"jiaoag admin token rotate default             # 怀疑泄露时换掉",
			"jiaoag admin token revoke viewer -yes",
		},
	}
}

func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		writeTokenHelp(stdout)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return tokenList(rest, stdout, stderr)
	case "create":
		return tokenCreate(rest, stdout, stderr)
	case "rotate":
		return tokenRotate(rest, stdout, stderr)
	case "revoke":
		return tokenRevoke(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "jiaoag admin token: 未知子命令 %q\n\n", sub)
		writeTokenHelp(stderr)
		return 2
	}
}

func writeTokenHelp(w io.Writer) {
	fs := flag.NewFlagSet("jiaoag admin token", flag.ContinueOnError)
	helppkg.Write(w, tokenHelp(fs))
}

// splitName 把位置参数（令牌名）从标志里摘出来，位置随意：
// `token create ops -read-only` 与 `token create -read-only ops` 都认。
// flag 包遇到第一个非标志就停止解析，之前就是因此把 -config 丢了。
func splitName(args []string) (name string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-config" || a == "--config":
			rest = append(rest, a)
			if i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-"):
			rest = append(rest, a)
		case name == "":
			name = a
		default:
			rest = append(rest, a)
		}
	}
	return name, rest
}

// openTokenStore 打开可写的数据库：令牌是运维动作，直接改库比「先起服务端
// 再走 WS」简单，多运行时同时写也是支持的（WAL + 写锁）。
func openTokenStore(cfgPath string, stderr io.Writer) (*store.Store, *config.Config, func(), int) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return nil, nil, nil, 1
	}
	if strings.TrimSpace(cfg.Storage.DBPath) == "" {
		fmt.Fprintln(stderr, "jiaoag admin token: 配置里没有 storage.db_path")
		return nil, nil, nil, 1
	}
	// 与 prune 同理：admin 不经过 ValidateServer，~ 要自己展开。
	dbPath, err := config.ExpandHome(cfg.Storage.DBPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return nil, nil, nil, 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		stop()
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return nil, nil, nil, 1
	}
	return db, cfg, func() { _ = db.Close(); stop() }, 0
}

func tokenList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag admin token list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	db, cfg, done, code := openTokenStore(*cfgPath, stderr)
	if db == nil {
		return code
	}
	defer done()

	rows, err := db.ServerTokens(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "库 %s\n", cfg.Storage.DBPath)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "还没有令牌（下次启动会把配置里的 server.token(s) 导进来）")
		return 0
	}
	fmt.Fprintf(stdout, "%-16s %-6s %-12s %-19s %-19s %s\n",
		"名字", "权限", "哈希前缀", "创建", "最近轮换", "状态")
	for _, r := range rows {
		perm := "读写"
		if r.ReadOnly {
			perm = "只读"
		}
		rotated := "-"
		if !r.RotatedAt.IsZero() {
			rotated = r.RotatedAt.Local().Format("2006-01-02 15:04")
		}
		state := "有效"
		if r.Revoked {
			state = "已吊销"
		}
		prefix := r.Hash
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		fmt.Fprintf(stdout, "%-16s %-6s %-12s %-19s %-19s %s\n",
			r.Name, perm, prefix, r.CreatedAt.Local().Format("2006-01-02 15:04"), rotated, state)
	}
	return 0
}

func tokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag admin token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	readOnly := fs.Bool("read-only", false, "只读令牌：能订阅/导出/抓指标，不能提交任务或裁决审批")
	name, rest := splitName(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		fmt.Fprintln(stderr, "jiaoag admin token create: 缺少令牌名（例如 jiaoag admin token create viewer -read-only）")
		return 2
	}
	db, _, done, code := openTokenStore(*cfgPath, stderr)
	if db == nil {
		return code
	}
	defer done()

	value, err := newTokenValue()
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return 1
	}
	if err := db.CreateServerToken(context.Background(), name, serverpkg.HashToken(value), *readOnly); err != nil {
		if errors.Is(err, store.ErrTokenExists) {
			fmt.Fprintf(stderr, "jiaoag admin token: 令牌 %q 已存在（要换明文用 rotate）\n", name)
			return 1
		}
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return 1
	}
	printNewToken(stdout, "已创建", name, value)
	return 0
}

func tokenRotate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag admin token rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	name, rest := splitName(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		fmt.Fprintln(stderr, "jiaoag admin token rotate: 缺少令牌名")
		return 2
	}
	db, _, done, code := openTokenStore(*cfgPath, stderr)
	if db == nil {
		return code
	}
	defer done()

	value, err := newTokenValue()
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return 1
	}
	if err := db.RotateServerToken(context.Background(), name, serverpkg.HashToken(value)); err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return 1
	}
	printNewToken(stdout, "已轮换", name, value)
	return 0
}

func tokenRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag admin token revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	yes := fs.Bool("yes", false, "确认吊销")
	name, rest := splitName(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		fmt.Fprintln(stderr, "jiaoag admin token revoke: 缺少令牌名")
		return 2
	}
	if !*yes {
		fmt.Fprintf(stdout, "吊销 %q 后，用它建立的连接立刻失效、新连接一律 401（记录保留，便于追责）。确认请加 -yes\n", name)
		return 1
	}
	db, _, done, code := openTokenStore(*cfgPath, stderr)
	if db == nil {
		return code
	}
	defer done()

	if err := db.RevokeServerToken(context.Background(), name); err != nil {
		fmt.Fprintf(stderr, "jiaoag admin token: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "已吊销 %q；正在运行的服务端 2 秒内生效\n", name)
	return 0
}

// printNewToken 是明文唯一一次出现的地方：库里只有哈希，之后再想拿回来
// 只能再轮换一次。
func printNewToken(w io.Writer, verb, name, value string) {
	fmt.Fprintf(w, "%s令牌 %q（明文只显示这一次，库里只存哈希）：\n\n  %s\n\n", verb, name, value)
	fmt.Fprintln(w, "把它填进客户端配置的 server.token，或用 -token 传；正在运行的服务端 2 秒内生效")
}

func newTokenValue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
