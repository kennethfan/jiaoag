// Package config 实现 `jiaoag config`：查看、校验与修改宿主配置。
//
// 它刻意不做结构性编辑（新增 provider/webhook 条目请用 config edit）：用
// 命令拼 YAML 列表的语法会迅速变成一套自己的 DSL，而 $EDITOR 已经够用。
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	"github.com/kennethfan/jiaoag/internal/config"
)

// Main 是子命令入口。
func Main() { os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin)) }

// Run 便于测试：返回退出码而不是直接退出。
func Run(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	if len(args) == 0 {
		writeHelp(stdout)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "path":
		return runPath(rest, stdout, stderr)
	case "show":
		return runShow(rest, stdout, stderr)
	case "check":
		return runCheck(rest, stdout, stderr)
	case "set":
		return runSet(rest, stdout, stderr, stdin)
	case "edit":
		return runEdit(rest, stdout, stderr)
	case "help", "-h", "--help":
		writeHelp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "jiaoag config: 未知子命令 %q\n\n", sub)
		writeHelp(stderr)
		return 2
	}
}

func writeHelp(w io.Writer) {
	fmt.Fprint(w, `jiaoag config — 查看、校验与修改配置

用法：
  jiaoag config path                                   打印生效的配置文件路径
  jiaoag config show [-format text|json]               生效值及其来源（密钥脱敏）
  jiaoag config check [-role server|client] [-online]  离线自检；--online 探测供应商/MCP/TLS
  jiaoag config set <path> <value|-> [-y]              改一个字段；默认只打印 diff
  jiaoag config edit                                   用 $EDITOR 打开，保存后自动校验

示例：
  jiaoag config show
  jiaoag config set agent.max_rounds 0 -y
  jiaoag config set server.token - -y          # 从 stdin 读，避免进 shell 历史
  jiaoag config set providers[0].api_key @/path/to/key -y
  jiaoag config check -online

路径写法与结构体一致：providers[0].model、notify.webhooks[1].url、mcp.servers[0].env.HOME。
敏感项（api_key/token/secret/value）只接受 - 或 @file。
`)
}

func runPath(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag config path", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p := *cfgPath
	if p == "" {
		p = config.DefaultPath()
	}
	expanded, err := config.ExpandHome(p)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag config path: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, expanded)
	if _, err := os.Stat(expanded); err != nil {
		fmt.Fprintln(stderr, "（文件不存在；先运行 `jiaoag serve -init`）")
	}
	return 0
}

func runShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	format := fs.String("format", "text", "输出格式：text 或 json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	file, fields, err := config.Show(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag config show: %v\n", err)
		return 1
	}
	switch *format {
	case "json":
		payload := struct {
			File   string              `json:"file"`
			Fields []config.ShownField `json:"fields"`
		}{File: file, Fields: fields}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(stderr, "jiaoag config show: %v\n", err)
			return 1
		}
	case "text", "":
		fmt.Fprintf(stdout, "# %s\n", file)
		for _, f := range fields {
			fmt.Fprintf(stdout, "%-44s %-32s # %s\n", f.Path, f.Value, f.Source)
		}
	default:
		fmt.Fprintf(stderr, "jiaoag config show: -format 只支持 text 或 json\n")
		return 2
	}
	return 0
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag config check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	role := fs.String("role", "server", "按哪个角色校验：server 或 client")
	online := fs.Bool("online", false, "探测供应商 /models、MCP server 与 TLS 握手")
	provider := fs.String("provider", "", "只探测指定供应商（默认全部）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	file, issues, cfg, err := config.Check(*cfgPath, *role)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag config check: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "# %s（role=%s）\n", file, *role)
	bad := 0
	for _, is := range issues {
		mark := "✗"
		if is.Level == "warn" {
			mark = "!"
		} else {
			bad++
		}
		field := is.Field
		if field != "" {
			field += ": "
		}
		fmt.Fprintf(stdout, "%s %s%s\n", mark, field, is.Message)
	}
	if len(issues) == 0 {
		fmt.Fprintln(stdout, "✓ 配置可用")
	}
	if *online && cfg != nil {
		fails := runOnline(cfg, *provider, stdout, stderr)
		bad += fails
	}
	if bad > 0 {
		return 1
	}
	return 0
}

func runSet(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	var (
		cfgPath string
		apply   bool
		pos     []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-y" || a == "--yes":
			apply = true
		case a == "-config" || a == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "jiaoag config set: -config 需要一个路径")
				return 2
			}
			i++
			cfgPath = args[i]
		case strings.HasPrefix(a, "-config="):
			cfgPath = strings.TrimPrefix(a, "-config=")
		case a == "-h" || a == "--help":
			fmt.Fprintln(stdout, "用法：jiaoag config set <path> <value|-> [@file] [-y]")
			return 0
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(stderr, "jiaoag config set: 未知参数 %q\n", a)
			return 2
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "用法：jiaoag config set <path> <value|-> [-y]（路径用 config show 查看）")
		return 2
	}
	key, raw := pos[0], pos[1]
	fromStream := false
	switch {
	case raw == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "jiaoag config set: 读取 stdin 失败: %v\n", err)
			return 1
		}
		raw = strings.TrimRight(string(b), "\r\n")
		fromStream = true
	case strings.HasPrefix(raw, "@"):
		b, err := os.ReadFile(strings.TrimPrefix(raw, "@"))
		if err != nil {
			fmt.Fprintf(stderr, "jiaoag config set: 读取文件失败: %v\n", err)
			return 1
		}
		raw = strings.TrimRight(string(b), "\r\n")
		fromStream = true
	}
	// 敏感项只接受流式输入：位置参数会留在 shell 历史与 ps 里。
	if !fromStream {
		file := cfgPath
		if file == "" {
			file = config.DefaultPath()
		}
		if cfg, err := config.Load(file); err == nil {
			if field, ok, _ := config.FindField(cfg, key); ok && field.Secret {
				fmt.Fprintf(stderr, "jiaoag config set: %s 是敏感项，请用 `-`（stdin）或 @file 传入\n", key)
				return 2
			}
		}
	}
	res, err := config.Set(config.SetOptions{Path: cfgPath, Key: key, Value: raw, Apply: apply})
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag config set: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "# %s\n%s\n", res.File, strings.TrimRight(res.Diff, "\n"))
	if !res.Changed {
		return 0
	}
	if res.Backup != "" {
		fmt.Fprintf(stdout, "已写入 %s（备份 %s）\n", res.File, res.Backup)
	} else {
		fmt.Fprintln(stdout, "（未写入；确认后加 -y 应用）")
	}
	return 0
}

func runEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jiaoag config edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	editor := fs.String("editor", "", "编辑器（默认 $VISUAL / $EDITOR / vi）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	file := *cfgPath
	if file == "" {
		file = config.DefaultPath()
	}
	expanded, err := config.ExpandHome(file)
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag config edit: %v\n", err)
		return 1
	}
	bin := *editor
	if bin == "" {
		bin = os.Getenv("VISUAL")
	}
	if bin == "" {
		bin = os.Getenv("EDITOR")
	}
	if bin == "" {
		bin = "vi"
	}
	cmd := exec.Command(bin, expanded)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "jiaoag config edit: 编辑器退出失败: %v\n", err)
		return 1
	}
	_, issues, _, err := config.Check(expanded, "server")
	if err != nil {
		fmt.Fprintf(stderr, "jiaoag config edit: 保存后的配置无法加载: %v\n", err)
		return 1
	}
	bad := 0
	for _, is := range issues {
		mark := "✗"
		if is.Level == "warn" {
			mark = "!"
		} else {
			bad++
		}
		fmt.Fprintf(stdout, "%s %s%s\n", mark, is.Field, is.Message)
	}
	if bad > 0 {
		return 1
	}
	fmt.Fprintln(stdout, "✓ 配置可用")
	return 0
}

var _ = helppkg.Spec{}
