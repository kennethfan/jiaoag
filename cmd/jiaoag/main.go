// jiaoag 是唯一的命令行入口：服务端、TUI、会话管理、事件导出、规则 feed
// 与开发用工具都是子命令。
//
// 之前它们是七个独立二进制（jiaoag-server / jiaoag-tui / jiaoag-events …），
// 名字本身就是提醒「你还得再去记一个命令」，而实际高频的只有一个——直接开
// TUI。收成一个入口后，裸跑就是 TUI，其余按需 `jiaoag <子命令>`。
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/kennethfan/jiaoag/internal/cli/config"
	"github.com/kennethfan/jiaoag/internal/cli/demo"
	"github.com/kennethfan/jiaoag/internal/cli/events"
	"github.com/kennethfan/jiaoag/internal/cli/pluginexample"
	"github.com/kennethfan/jiaoag/internal/cli/policy"
	"github.com/kennethfan/jiaoag/internal/cli/serve"
	"github.com/kennethfan/jiaoag/internal/cli/sessions"
	"github.com/kennethfan/jiaoag/internal/cli/tui"
)

const usageText = `jiaoag — 单用户编码 agent

用法：
  jiaoag [TUI 参数]              打开 TUI（等同 jiaoag tui）
  jiaoag serve [参数]            启动服务端（默认 127.0.0.1:8788）
  jiaoag tui [参数]              显式打开 TUI
  jiaoag sessions list|rename|rm 在终端里管理会话
  jiaoag events [参数]           订阅并导出事件与审计（JSONL）
  jiaoag policy sync|verify|test 拉取并验签规则 feed，或解释一条命令的判定
  jiaoag config path|show|check  查看与校验配置（set/edit 可修改）
  jiaoag demo [参数]             真实 provider 冒烟（开发用）
  jiaoag plugin-example          参考插件，走 stdio JSON-RPC（开发用）
  jiaoag help                    显示这段说明

各子命令的参数用 ` + "`jiaoag <子命令> -h`" + ` 查看。`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		dispatch("tui", nil)
		return
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Println(usageText)
		return
	}
	// 以 - 开头说明用户想跑 TUI 并且带了它的参数（jiaoag -c / jiaoag -s ses_x），
	// 不能当成未知子命令。
	if strings.HasPrefix(args[0], "-") {
		dispatch("tui", args)
		return
	}
	switch args[0] {
	case "serve", "tui", "events", "sessions", "policy", "config", "demo", "plugin-example":
		dispatch(args[0], args[1:])
	default:
		fmt.Fprintf(os.Stderr, "jiaoag: 未知子命令 %q\n\n%s\n", args[0], usageText)
		os.Exit(2)
	}
}

// dispatch 重写 os.Args 后交给子命令：各子命令仍按常规方式解析 os.Args，
// 程序名写成 "jiaoag <子命令>"，usage 与报错前缀因此一致。
func dispatch(sub string, rest []string) {
	os.Args = append([]string{"jiaoag " + sub}, rest...)
	switch sub {
	case "serve":
		serve.Main()
	case "tui":
		tui.Main()
	case "sessions":
		sessions.Main()
	case "events":
		events.Main()
	case "policy":
		policy.Main()
	case "config":
		config.Main()
	case "demo":
		demo.Main()
	case "plugin-example":
		pluginexample.Main()
	}
}
