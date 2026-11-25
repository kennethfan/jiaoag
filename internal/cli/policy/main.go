// Command jiaoag policy 同步签名规则 feed。
//
// 它只做一件事：下载 → 验签 → 挡回滚 → 原子写入 policy.d/feed.yaml（及 .sig）。
// 规则怎么生效由服务端启动时读取，所以更新时机是显式的：跑一次命令，重启或
// 让下次启动生效。服务端不主动出网。
package policy

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/policyfeed"
)

func Main() {
	args := os.Args[1:]
	// 允许把动作写成子命令（sync / verify），也允许 -verify 这种纯 flag 形式。
	// 注意 flag.Parse 遇到第一个非 flag 参数就停，所以子命令必须先摘掉。
	verifyMode := false
	if len(args) > 0 {
		switch args[0] {
		case "sync":
			args = args[1:]
		case "verify", "check":
			verifyMode = true
			args = args[1:]
		case "help", "-h", "--help":
			fs := flag.NewFlagSet("jiaoag policy", flag.ExitOnError)
			declarePolicyFlags(fs, false)
			helppkg.Write(os.Stdout, policyHelp(fs))
			return
		}
	}
	// 什么都不给时给帮助：默认动作是「联网安装」太冒险，不该靠猜。
	if len(args) == 0 {
		fs := flag.NewFlagSet("jiaoag policy", flag.ExitOnError)
		declarePolicyFlags(fs, false)
		helppkg.Write(os.Stdout, policyHelp(fs))
		return
	}
	if err := run(args, verifyMode); err != nil {
		fmt.Fprintf(os.Stderr, "jiaoag policy: %v\n", err)
		os.Exit(1)
	}
}

type keyList []string

func (k *keyList) String() string { return strings.Join(*k, ",") }

func (k *keyList) Set(v string) error {
	*k = append(*k, v)
	return nil
}

func run(args []string, verifyMode bool) error {
	fs := flag.NewFlagSet("jiaoag policy", flag.ExitOnError)
	vars := declarePolicyFlags(fs, verifyMode)
	fs.Usage = func() { helppkg.Write(os.Stdout, policyHelp(fs)) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*vars.cfgPath)
	if err != nil {
		return err
	}

	targetDir := strings.TrimSpace(*vars.dir)
	if targetDir == "" {
		targetDir, err = config.ExpandHome(cfg.Policy.Dir)
		if err != nil {
			return err
		}
	}
	if targetDir == "" {
		return fmt.Errorf("没有规则目录：配置 policy.dir 或用 -dir 指定")
	}

	if len(*vars.keys) > 0 {
		cfg.Policy.Feed.Keys = *vars.keys
	}
	pub, err := cfg.FeedKeys()
	if err != nil {
		return err
	}
	if len(pub) == 0 {
		return fmt.Errorf("没有公钥：配置 policy.feed.keys 或用 -key 指定")
	}

	if *vars.verify {
		_, meta, err := policyfeed.LoadFile(targetDir, pub)
		if err != nil {
			return err
		}
		fmt.Printf("feed 有效：version=%s 签发=%s 过期=%s\n",
			meta.Version, fmtTime(meta.IssuedAt), fmtTime(meta.ExpiresAt))
		return expiredWarning(meta)
	}

	src := strings.TrimSpace(*vars.url)
	if src == "" {
		src = strings.TrimSpace(cfg.Policy.Feed.URL)
	}
	if src == "" {
		return fmt.Errorf("没有 feed 地址：配置 policy.feed.url 或用 -url 指定")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := policyfeed.Sync(ctx, policyfeed.Options{
		URL:   src,
		Keys:  pub,
		Dir:   targetDir,
		Force: *vars.force,
	})
	if err != nil {
		return err
	}
	if !res.Changed {
		fmt.Printf("已是最新：version=%s sha256=%s\n", res.Version, res.SHA256[:12])
		return nil
	}
	fmt.Printf("已更新：version=%s 签发=%s 过期=%s sha256=%s\n  → %s\n",
		res.Version, fmtTime(res.IssuedAt), fmtTime(res.ExpiresAt), res.SHA256[:12], res.Path)
	fmt.Println("服务端下次启动会加载这份规则；已在运行的服务端需要重启才生效。")
	return nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "无"
	}
	return t.Format(time.RFC3339)
}

func expiredWarning(meta policyfeed.Metadata) error {
	if !meta.ExpiresAt.IsZero() && time.Now().After(meta.ExpiresAt) {
		fmt.Fprintf(os.Stderr, "jiaoag policy: 注意：feed 已于 %s 过期，服务端将拒绝启动，请重新同步\n",
			fmtTime(meta.ExpiresAt))
	}
	return nil
}

// policyFlags 集中保存各标志指针；帮助文本与 run() 共用同一份声明，
// 否则 -h 里列出的标志与真正解析的会慢慢对不上。
type policyFlags struct {
	cfgPath *string
	url     *string
	dir     *string
	verify  *bool
	force   *bool
	keys    *keyList
}

func declarePolicyFlags(fs *flag.FlagSet, verifyMode bool) policyFlags {
	var out policyFlags
	out.cfgPath = fs.String("config", "", "配置文件（默认 ~/.config/jiaoag/config.yaml）")
	out.url = fs.String("url", "", "覆盖 policy.feed.url")
	out.dir = fs.String("dir", "", "覆盖 policy.d 目录")
	out.verify = fs.Bool("verify", verifyMode, "只校验本地已安装的 feed，不联网")
	out.force = fs.Bool("force", false, "允许安装过期的 feed 或撤销回滚保护")
	var keys keyList
	fs.Var(&keys, "key", "覆盖 policy.feed.keys（ed25519 公钥 base64，可重复）")
	out.keys = &keys
	return out
}

func policyHelp(fs *flag.FlagSet) helppkg.Spec {
	return helppkg.Spec{
		Summary: "jiaoag policy 安装与校验签名的权限规则 feed；feed 只能收紧规则，不能放宽。",
		Usage: []string{
			"jiaoag policy sync [-url 地址] [-key 公钥]... [-force]",
			"jiaoag policy verify [-dir 目录]",
		},
		Commands: []helppkg.Command{
			{Name: "sync", Desc: "从 url 拉取 feed，验签、挡回滚、写 policy.d"},
			{Name: "verify", Desc: "只校验已安装的 feed，不联网"},
		},
		Flags: fs,
		Examples: []string{
			"jiaoag policy sync",
			"jiaoag policy verify",
			"jiaoag policy sync -url file:///tmp/feed.yaml -key $(cat feed.pub)",
		},
	}
}
