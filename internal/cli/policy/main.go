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
		}
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
	var (
		cfgPath = fs.String("config", "", "配置文件（默认 ~/.config/jiaoag/config.yaml）")
		url     = fs.String("url", "", "覆盖 policy.feed.url")
		dir     = fs.String("dir", "", "覆盖 policy.d 目录")
		verify  = fs.Bool("verify", verifyMode, "只校验本地已安装的 feed，不联网")
		force   = fs.Bool("force", false, "允许安装过期的 feed 或撤销回滚保护")
		keys    keyList
	)
	fs.Var(&keys, "key", "覆盖 policy.feed.keys（ed25519 公钥 base64，可重复）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	targetDir := strings.TrimSpace(*dir)
	if targetDir == "" {
		targetDir, err = config.ExpandHome(cfg.Policy.Dir)
		if err != nil {
			return err
		}
	}
	if targetDir == "" {
		return fmt.Errorf("没有规则目录：配置 policy.dir 或用 -dir 指定")
	}

	if len(keys) > 0 {
		cfg.Policy.Feed.Keys = keys
	}
	pub, err := cfg.FeedKeys()
	if err != nil {
		return err
	}
	if len(pub) == 0 {
		return fmt.Errorf("没有公钥：配置 policy.feed.keys 或用 -key 指定")
	}

	if *verify {
		_, meta, err := policyfeed.LoadFile(targetDir, pub)
		if err != nil {
			return err
		}
		fmt.Printf("feed 有效：version=%s 签发=%s 过期=%s\n",
			meta.Version, fmtTime(meta.IssuedAt), fmtTime(meta.ExpiresAt))
		return expiredWarning(meta)
	}

	src := strings.TrimSpace(*url)
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
		Force: *force,
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
