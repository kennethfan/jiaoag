package policy

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// runTest 解释一条命令会被怎么判定：放行、还是要审批、命中的是哪条规则。
//
// 它只读规则、不改任何东西；存在的理由是白名单现在归用户编辑，
// 「我这样写对不对」需要一个不必跑任务就能验证的入口。
func runTest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag policy test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	dir := fs.String("policy-dir", "", "规则目录（默认配置里的 policy.dir）")
	shell := fs.String("shell", "", "按 shell 形态判定（管道/重定向固定需要审批）")

	// `--` 之后是命令 argv；Go 的 flag 遇到第一个非 flag 就停，所以先切开。
	split := len(args)
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	if err := fs.Parse(args[:split]); err != nil {
		return 2
	}
	argv := args[split:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 && *shell == "" {
		fmt.Fprintln(stderr, "用法：jiaoag policy test -- <命令 argv…>  或  jiaoag policy test --shell 'ls | wc -l'")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag policy test: %v\n", err)
		return 1
	}
	targetDir := strings.TrimSpace(*dir)
	if targetDir == "" {
		targetDir, err = config.ExpandHome(cfg.Policy.Dir)
		if err != nil {
			fmt.Fprintf(stderr, "jiaoag policy test: %v\n", err)
			return 1
		}
	}
	keys, err := cfg.FeedKeys()
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag policy test: %v\n", err)
		return 1
	}
	var rs *perm.Ruleset
	// 只读命令不该顺手在配置目录里创建文件：目录不存在时按内置模板判定，
	// 并说明「真正生效前需要 serve 或 config check 落盘」。
	if _, statErr := os.Stat(targetDir); statErr != nil {
		rs = perm.DefaultRuleset()
		fmt.Fprintf(stdout, "提示：规则目录 %s 不存在，以下按内置模板 %s 判定\n", targetDir, rs.Version)
	} else {
		rs, err = perm.NewLoader(targetDir).WithFeedKeys(keys).Load()
		if err != nil {
			fmt.Fprintf(stderr, "jiaoag policy test: %v\n", err)
			return 1
		}
	}
	for _, w := range rs.Warnings {
		fmt.Fprintf(stdout, "提示：%s\n", w)
	}

	cwd, _ := os.Getwd()
	actual := argv
	if *shell != "" {
		// 与工具层一致：shell 形态在 argv 里表现为 sh -c <整条>，
		// 策略层据此识别并固定要审批。
		actual = []string{"sh", "-c", *shell}
	}
	prepared := tools.Prepared{
		Call: tools.Call{Name: "run_command"},
		Command: &tools.Command{
			Argv:  actual,
			Shell: *shell,
			Cwd:   cwd,
		},
	}
	d := rs.Evaluate(context.Background(), prepared)

	if *shell != "" {
		fmt.Fprintf(stdout, "命令：%s（shell 形态）\n", *shell)
	} else {
		fmt.Fprintf(stdout, "命令：%s\n", strings.Join(argv, " "))
	}
	verdict := "require_user（需要审批）"
	if d.Verdict == perm.Allow {
		verdict = "allow（放行）"
	}
	fmt.Fprintf(stdout, "判定：%s\n", verdict)
	if d.RuleID != "" {
		fmt.Fprintf(stdout, "规则：%s\n", d.RuleID)
	}
	short := rs.Hash
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(stdout, "策略：%s（hash %s）\n", rs.Version, short)
	if d.Reason != "" {
		fmt.Fprintf(stdout, "原因：%s\n", d.Reason)
	}
	fmt.Fprintf(stdout, "目录：%s\n", targetDir)
	if d.Verdict == perm.Allow {
		return 0
	}
	return 1
}
