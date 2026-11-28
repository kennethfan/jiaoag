// Command jiaoag admin 是运维子命令的入口：prune（按保留期删除过期会话）
// 与 token（访问令牌的增删改）。
//
// 为什么不做成服务端命令：清理是低频动作，不需要 WS 协议、令牌或一个正在
// 跑的服务端；直接打开数据库就够了。多运行时同时写是支持的（WAL），删除
// 与任务占位在同一套 SQLite 写锁下排队。
package admin

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kennethfan/jiaoag/internal/app"
	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/store"
)

func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run 是 Main 的可测试版本：不调 os.Exit，输出走传入的流。
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeHelp(stdout)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeHelp(stdout)
		return 0
	case "prune":
		return runPrune(args[1:], stdout, stderr)
	case "token":
		return runToken(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "jiaoag admin: 未知子命令 %q\n\n", args[0])
		writeHelp(stderr)
		return 2
	}
}

func writeHelp(w io.Writer) {
	helppkg.Write(w, helppkg.Spec{
		Summary: "jiaoag admin 是运维子命令：按保留期清理过期会话。",
		Usage: []string{
			"jiaoag admin prune [-older-than 90d] [-dry-run | -yes]",
		},
		Commands: []helppkg.Command{
			{Name: "prune", Desc: "删除超过保留期、且所有任务都已结束的会话（连同任务/事件/审计）"},
			{Name: "token", Desc: "管理访问令牌：list / create / rotate / revoke（改完 2 秒内生效）"},
		},
		Examples: []string{
			"jiaoag admin prune -older-than 90d -dry-run   # 先看会删哪些",
			"jiaoag admin prune -older-than 90d -yes       # 真删",
			"jiaoag admin prune -older-than 30d -yes -config ~/.config/jiaoag/config.yaml",
		},
	})
}

func pruneHelp(fs *flag.FlagSet) helppkg.Spec {
	return helppkg.Spec{
		Summary: "删除超过保留期的会话：整块移除任务、事件、审计、审批与工具调用。",
		Usage:   []string{"jiaoag admin prune [-older-than 90d] [-dry-run | -yes]"},
		Flags:   fs,
		Examples: []string{
			"jiaoag admin prune -older-than 90d -dry-run",
			"jiaoag admin prune -older-than 90d -yes",
		},
	}
}

func runPrune(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag admin prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	olderThan := fs.String("older-than", "90d", "保留期：30d / 720h / 90（纯数字按天）")
	dryRun := fs.Bool("dry-run", false, "只列出会被删除的会话，不动数据")
	yes := fs.Bool("yes", false, "确认执行删除")
	fs.Usage = func() { helppkg.Write(stdout, pruneHelp(fs)) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	retention, err := parseRetention(*olderThan)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin prune: %v\n", err)
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin prune: %v\n", err)
		return 1
	}
	if strings.TrimSpace(cfg.Storage.DBPath) == "" {
		fmt.Fprintln(stderr, "jiaoag admin prune: 配置里没有 storage.db_path")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 这里不走 ValidateServer，所以 ~ 要自己展开：否则会在当前目录下
	// 建一个字面量名为 "~" 的目录（踩过一次）。
	dbPath, err := config.ExpandHome(cfg.Storage.DBPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin prune: %v\n", err)
		return 1
	}
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin prune: %v\n", err)
		return 1
	}
	application := app.New(db, event.NewHub())
	defer application.Close()

	cands, err := application.PruneCandidates(ctx, retention, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin prune: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "库 %s，保留期 %s\n", cfg.Storage.DBPath, humanRetention(retention))
	if len(cands) == 0 {
		fmt.Fprintln(stdout, "没有需要清理的会话")
		return 0
	}

	fmt.Fprintf(stdout, "找到 %d 个过期会话：\n", len(cands))
	for _, c := range cands {
		title := c.Title
		if title == "" {
			title = "(未命名)"
		}
		fmt.Fprintf(stdout, "  %s  %s  %d 个任务  %s  %s\n",
			c.ID, c.UpdatedAt.Local().Format("2006-01-02 15:04"), c.Tasks, title, c.Workspace)
	}

	if *dryRun {
		fmt.Fprintln(stdout, "（-dry-run：未删除任何数据）")
		return 0
	}
	if !*yes {
		fmt.Fprintln(stdout, "删除会连带清掉这些会话的任务、事件、审计与审批，且不可撤销；确认请加 -yes")
		return 1
	}

	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	deleted, err := application.PruneSessions(ctx, ids)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag admin prune: 部分会话未删除: %v\n", err)
	}
	fmt.Fprintf(stdout, "已删除 %d 个会话\n", deleted)
	if err != nil {
		return 1
	}
	return 0
}

// parseRetention 接受 30d / 720h / 45m / 90（纯数字表示天）。
func parseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("-older-than 不能为空")
	}
	if days, err := strconv.Atoi(s); err == nil {
		if days <= 0 {
			return 0, fmt.Errorf("-older-than 必须大于 0，当前为 %d", days)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("无法解析 -older-than %q（示例：90d / 720h / 90）", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("无法解析 -older-than %q（示例：90d / 720h / 90）", s)
	}
	return d, nil
}

func humanRetention(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d 天", int(d/(24*time.Hour)))
	}
	return d.String()
}
