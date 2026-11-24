// Command jiaoag sessions 在会话之外管理会话：列、改名、删除。
//
// TUI 里的 /sessions 面板只能在某个会话里打开，命令行工具没有会话上下文，
// 所以列表要么按目录筛（默认当前目录），要么用 -all 跨工作区全列。
//
// 它通过 WS 协议说话而不是直接读 SQLite：改名要写 session.renamed 事件、
// 删除要受「有未结束任务的会话不许删」保护，这两件事只有服务端知道。
package sessions

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func Main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// 子命令之前的全局标志先剥掉：`jiaoag sessions -config x list` 与
	// `jiaoag sessions list -config x` 都该能用。flag 包遇到第一个位置参数就停，
	// 不先剥的话前者会把 "-config" 当成子命令。
	rest := os.Args[1:]
	var pre globalFlags
	if strings.HasPrefix(rest[0], "-") {
		pre = parseGlobal(rest)
		rest = pre.args
		if len(rest) == 0 {
			usage()
			os.Exit(2)
		}
	}
	command := rest[0]
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	fs.Usage = usage
	cfgPath := fs.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
	urlFlag := fs.String("url", "", "服务端 WS 地址（默认由 server.listen 推导）")
	tokenFlag := fs.String("token", "", "覆盖 server.token")
	insecure := fs.Bool("insecure", false, "跳过 TLS 证书校验（仅用于自签证书）")

	var (
		all       *bool
		workspace *string
		format    *string
	)
	switch command {
	case "list":
		all = fs.Bool("all", false, "跨工作区列出全部会话")
		workspace = fs.String("workspace", "", "按目录筛选（默认当前目录）")
		format = fs.String("format", "text", "输出格式：text | json")
	case "rm", "rename":
	default:
		fmt.Fprintf(os.Stderr, "jiaoag sessions: 未知子命令 %q\n\n", command)
		usage()
		os.Exit(2)
	}
	// -yes 允许写在末尾（`rm <id> -yes` 比 `rm -yes <id>` 自然），先摘掉它，
	// 免得 flag 在第一个位置参数处停下后把它当成多余的参数。
	yesGiven := false
	if command == "rm" {
		rest, yesGiven = stripYes(rest)
	}

	// 子命令自己的标志仍要写在位置参数前面：flag 遇到第一个非标志就停。
	_ = fs.Parse(rest[1:])
	args := fs.Args()

	// 子命令后的写法优先，其次才是子命令前的。
	pre.apply(cfgPath, urlFlag, tokenFlag, insecure)
	opts, err := resolve(*cfgPath, *urlFlag, *tokenFlag, *insecure)
	if err != nil {
		fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := dial(ctx, opts)
	if err != nil {
		fatal(err)
	}
	defer conn.CloseNow()

	switch command {
	case "list":
		dir := *workspace
		if dir == "" && !*all {
			dir, err = os.Getwd()
			if err != nil {
				fatal(err)
			}
		}
		// 服务端也会绝对化，但它按自己的 cwd 算，相对路径必须在客户端先定型。
		if dir != "" {
			dir, err = filepath.Abs(dir)
			if err != nil {
				fatal(err)
			}
		}
		summaries, err := list(ctx, conn, dir, *all)
		if err != nil {
			fatal(err)
		}
		if *format == "json" {
			out, err := json.MarshalIndent(summaries, "", "  ")
			if err != nil {
				fatal(err)
			}
			fmt.Println(string(out))
			return
		}
		if *format != "text" {
			fatal(fmt.Errorf("未知输出格式 %q（text | json）", *format))
		}
		printTable(summaries, *all)
	case "rename":
		if len(args) < 2 {
			fatal(errors.New("用法：jiaoag sessions rename <会话 id> <新标题>"))
		}
		title := strings.Join(args[1:], " ")
		id, err := resolveID(ctx, conn, args[0])
		if err != nil {
			fatal(err)
		}
		sess, err := rename(ctx, conn, id, title)
		if err != nil {
			fatal(err)
		}
		if sess.Title == "" {
			fmt.Printf("已清空标题：%s\n", sess.ID)
			return
		}
		fmt.Printf("已改名：%s → %s\n", sess.ID, sess.Title)
	case "rm":
		if len(args) != 1 {
			fatal(errors.New("用法：jiaoag sessions rm <会话 id> -yes"))
		}
		if !yesGiven {
			fatal(errors.New("删除会连带任务、事件与审计一起抹掉且不可撤销；确认请加 -yes"))
		}
		id, err := resolveID(ctx, conn, args[0])
		if err != nil {
			fatal(err)
		}
		if err := remove(ctx, conn, id); err != nil {
			fatal(err)
		}
		fmt.Printf("已删除 %s\n", id)
	}
}

// globalFlags 承接写在子命令之前的通用标志。
type globalFlags struct {
	cfg      string
	url      string
	token    string
	insecure bool
	args     []string
}

func parseGlobal(args []string) globalFlags {
	fs := flag.NewFlagSet("jiaoag sessions", flag.ExitOnError)
	cfg := fs.String("config", "", "配置文件路径")
	url := fs.String("url", "", "服务端 WS 地址")
	token := fs.String("token", "", "覆盖 server.token")
	insecure := fs.Bool("insecure", false, "跳过 TLS 证书校验")
	_ = fs.Parse(args)
	return globalFlags{cfg: *cfg, url: *url, token: *token, insecure: *insecure, args: fs.Args()}
}

// apply 只在子命令后没给时补位，避免前者覆盖后者。
func (g globalFlags) apply(cfg, url, token *string, insecure *bool) {
	if *cfg == "" {
		*cfg = g.cfg
	}
	if *url == "" {
		*url = g.url
	}
	if *token == "" {
		*token = g.token
	}
	if !*insecure {
		*insecure = g.insecure
	}
}

// stripYes 摘掉任意位置的 -yes/--yes，返回剩余参数与是否给了。
func stripYes(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	given := false
	for _, a := range args {
		if a == "-yes" || a == "--yes" {
			given = true
			continue
		}
		out = append(out, a)
	}
	return out, given
}

func usage() {
	fmt.Fprint(os.Stderr, `jiaoag sessions 在会话之外管理会话（标志写在位置参数之前）

  jiaoag sessions list [-all] [-workspace 目录] [-format text|json]
  （通用标志 -config/-url/-token/-insecure 写在子命令前后都行）
  jiaoag sessions rename <会话 id> <新标题>
  jiaoag sessions rm <会话 id> -yes

通用标志：-config <文件> -url <ws 地址> -token <令牌> -insecure
`)
}

// options 是连服务端需要的一切；与 jiaoag events 同形，便于两边一起读。
type options struct {
	url      string
	token    string
	insecure bool
}

func resolve(cfgPath, urlFlag, tokenFlag string, insecure bool) (options, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return options{}, err
	}
	if err := cfg.ValidateClient(); err != nil {
		return options{}, err
	}
	token := tokenFlag
	if token == "" {
		secret, err := cfg.Server.ClientToken()
		if err != nil {
			return options{}, err
		}
		resolved, err := secret.Resolve()
		if err != nil {
			return options{}, fmt.Errorf("解析 server.token 失败: %w", err)
		}
		token = resolved
	}
	target := urlFlag
	if target == "" {
		scheme := "ws"
		if cfg.Server.TLSEnabled() {
			scheme = "wss"
		}
		target = scheme + "://" + dialableAddr(cfg.Server.Listen) + "/ws"
	}
	return options{url: target, token: token, insecure: insecure}, nil
}

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

func dial(ctx context.Context, opts options) (*websocket.Conn, error) {
	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + opts.token}},
	}
	if opts.insecure {
		dialOpts.HTTPClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}
	conn, _, err := websocket.Dial(ctx, opts.url, dialOpts)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", opts.url, err)
	}
	var hello protocol.Envelope
	if err := wsjson.Read(ctx, conn, &hello); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("读 hello 失败: %w", err)
	}
	if hello.Type != protocol.TypeHello {
		conn.CloseNow()
		return nil, fmt.Errorf("期待 hello，收到 %s", hello.Type)
	}
	var hp protocol.HelloPayload
	if err := hello.Decode(&hp); err != nil {
		conn.CloseNow()
		return nil, err
	}
	if hp.ProtocolVersion != protocol.Version {
		conn.CloseNow()
		return nil, fmt.Errorf("协议版本不匹配：服务端 %d，客户端 %d", hp.ProtocolVersion, protocol.Version)
	}
	env, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion: "jiaoag sessions", ProtocolVersion: protocol.Version,
	})
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	if err := wsjson.Write(ctx, conn, env); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("发送 client.hello 失败: %w", err)
	}
	return conn, nil
}

func list(ctx context.Context, conn *websocket.Conn, workspace string, all bool) ([]protocol.SessionSummaryPayload, error) {
	payload := protocol.SessionManagePayload{All: all}
	if !all {
		payload.Workspace = workspace
	}
	if err := send(ctx, conn, protocol.TypeSessionManage, payload); err != nil {
		return nil, err
	}
	env, err := await(ctx, conn, protocol.TypeSessionManageResult)
	if err != nil {
		return nil, err
	}
	var p protocol.SessionManageResultPayload
	if err := env.Decode(&p); err != nil {
		return nil, err
	}
	return p.Sessions, nil
}

func rename(ctx context.Context, conn *websocket.Conn, id, title string) (protocol.SessionPayload, error) {
	if err := send(ctx, conn, protocol.TypeSessionRename, protocol.SessionRenamePayload{SessionID: id, Title: title}); err != nil {
		return protocol.SessionPayload{}, err
	}
	env, err := await(ctx, conn, protocol.TypeSessionRenamed)
	if err != nil {
		return protocol.SessionPayload{}, err
	}
	var sess protocol.SessionPayload
	if err := env.Decode(&sess); err != nil {
		return protocol.SessionPayload{}, err
	}
	return sess, nil
}

func remove(ctx context.Context, conn *websocket.Conn, id string) error {
	if err := send(ctx, conn, protocol.TypeSessionDelete, protocol.SessionDeletePayload{SessionID: id}); err != nil {
		return err
	}
	_, err := await(ctx, conn, protocol.TypeSessionDeleted)
	return err
}

// resolveID 把列表里看到的短 id 补全成完整 id：手抄 16 位十六进制没有意义，
// 而前缀在几十条会话里足以唯一。
func resolveID(ctx context.Context, conn *websocket.Conn, prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", errors.New("会话 id 不能为空")
	}
	summaries, err := list(ctx, conn, "", true)
	if err != nil {
		return "", err
	}
	var hit []string
	for _, s := range summaries {
		if s.ID == prefix {
			return s.ID, nil
		}
		if strings.HasPrefix(s.ID, prefix) {
			hit = append(hit, s.ID)
		}
	}
	switch len(hit) {
	case 0:
		return "", fmt.Errorf("没有匹配 %q 的会话", prefix)
	case 1:
		return hit[0], nil
	default:
		return "", fmt.Errorf("%q 匹配到 %d 条会话，请多给几位：%s", prefix, len(hit), strings.Join(hit, ", "))
	}
}

func send(ctx context.Context, conn *websocket.Conn, typ string, payload any) error {
	env, err := protocol.New(typ, payload)
	if err != nil {
		return err
	}
	if err := wsjson.Write(ctx, conn, env); err != nil {
		return fmt.Errorf("发送 %s 失败: %w", typ, err)
	}
	return nil
}

// await 等到指定类型的回帧。列表/改名之后服务端不会再主动推事件（这条连接
// 没订阅任何会话），但仍跳过其它帧，避免将来加推送时挤掉回执。
func await(ctx context.Context, conn *websocket.Conn, want string) (protocol.Envelope, error) {
	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return protocol.Envelope{}, fmt.Errorf("读取 %s 回执失败: %w", want, err)
		}
		if env.Type == protocol.TypeError {
			var e protocol.ErrorPayload
			if err := env.Decode(&e); err == nil {
				return protocol.Envelope{}, fmt.Errorf("%s: %s", e.Code, e.Message)
			}
			return protocol.Envelope{}, fmt.Errorf("服务端返回错误: %s", env.Payload)
		}
		if env.Type == want {
			return env, nil
		}
	}
}

func printTable(summaries []protocol.SessionSummaryPayload, withWorkspace bool) {
	if len(summaries) == 0 {
		fmt.Println("（没有会话）")
		return
	}
	home, _ := os.UserHomeDir()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if withWorkspace {
		fmt.Fprintln(w, "ID\t标题\t工作区\t任务\t进行中\t模型\t最后活动")
	} else {
		fmt.Fprintln(w, "ID\t标题\t任务\t进行中\t模型\t最后活动")
	}
	for _, s := range summaries {
		title := s.Title
		if title == "" {
			title = "(未命名)"
		}
		model := s.Model
		if s.Provider != "" && model != "" {
			model = s.Provider + "/" + model
		}
		if model == "" {
			model = "-"
		}
		active := "-"
		if s.ActiveTasks > 0 {
			active = fmt.Sprintf("%d", s.ActiveTasks)
		}
		if withWorkspace {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				shortID(s.ID), title, abbrevHome(s.Workspace, home), s.Tasks, active, model, when(s.UpdatedAt))
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			shortID(s.ID), title, s.Tasks, active, model, when(s.UpdatedAt))
	}
	_ = w.Flush()
}

// shortID 只留 ses_ 之后的前 8 位：完整 id 在 tabwriter 里会把标题挤到很远，
// 而 rm/rename 接受前缀也能补全（服务端按完整 id 查，见 resolveID）。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func abbrevHome(path, home string) string {
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// when 把毫秒时间戳压成「当天 HH:MM，否则 MM-DD」：列表要的是新旧，不是精确时刻。
func when(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	at := time.UnixMilli(ms).Local()
	now := time.Now()
	if at.Year() == now.Year() && at.YearDay() == now.YearDay() {
		return at.Format("15:04")
	}
	return at.Format("01-02")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "jiaoag sessions: %v\n", err)
	os.Exit(1)
}
