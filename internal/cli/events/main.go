// Command jiaoag events 是只读的事件与审计消费者：连上服务端，把一个会话的
// 事件流和全库审计增量打到 stdout，默认每行一条 JSON（便于接日志系统/CI）。
//
// 它不写任何东西：没有「删除审计」「改状态」的入口，协议侧也只有
// event.subscribe / audit.tail 两个只读命令。导出与监控是同一个程序，
// 区别只在 -from / -audit-from 和 -until-now。
package events

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
	launchpkg "github.com/kennethfan/jiaoag/internal/cli/launch"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

func Main() {
	var (
		cfgPath   = flag.String("config", "", "配置文件路径（默认 ~/.config/jiaoag/config.yaml）")
		url       = flag.String("url", "", "服务端 WS 地址（默认由 server.listen 推导）")
		token     = flag.String("token", "", "覆盖 server.token")
		sessionID string
		from      = flag.Int64("from", -1, "事件起始序号；-1 表示只看新事件，0 表示导出全部历史")
		auditFrom = flag.Int64("audit-from", -1, "审计起始 id；-1 表示只看新审计，0 表示导出全部历史")
		format    = flag.String("format", "json", "输出格式：json（每行一条）| text")
		untilNow  = flag.Bool("until-now", false, "导出到启动时刻为止后退出（适合定时导出）")
		interval  = flag.Duration("interval", time.Second, "审计轮询间隔")
		insecure  = flag.Bool("insecure", false, "跳过 TLS 证书校验（仅用于自签证书）")
		noSpawn   = flag.Bool("no-spawn", false, "连不上时不自动拉起本地服务端")
		serverBin = flag.String("server-bin", "", "拉起服务端用的可执行文件（默认就是自己）")
	)
	flag.StringVar(&sessionID, "s", "", "会话 id（默认取最近活跃的会话）")
	flag.StringVar(&sessionID, "session", "", "同 -s（保留旧写法）")
	flag.Usage = func() { helppkg.Write(os.Stdout, eventsHelp(flag.CommandLine)) }
	flag.Parse()
	if *format != "json" && *format != "text" {
		fmt.Fprintf(os.Stderr, "jiaoag events: 未知输出格式 %q（json | text）\n", *format)
		os.Exit(2)
	}
	if *interval < 100*time.Millisecond {
		*interval = 100 * time.Millisecond
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if err := cfg.ValidateClient(); err != nil {
		fatal(err)
	}
	if *token == "" {
		secret, err := cfg.Server.ClientToken()
		if err != nil {
			fatal(err)
		}
		resolved, err := secret.Resolve()
		if err != nil {
			fatal(fmt.Errorf("解析 server.token 失败: %w", err))
		}
		*token = resolved
	}

	target := *url
	if target == "" {
		scheme := "ws"
		if cfg.Server.TLSEnabled() {
			scheme = "wss"
		}
		target = scheme + "://" + dialableAddr(cfg.Server.Listen) + "/ws"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 只读导出同样不该要求先手工起服务端：本地连不上就拉一个，
	// 命令结束后服务端按自己的空闲倒计时退出。
	if !*noSpawn {
		cleanup, err := launchpkg.Ensure(ctx, cfg, *cfgPath, *serverBin, target)
		if err != nil {
			fatal(err)
		}
		defer cleanup()
	}

	opts := options{
		url: target, token: *token, sessionID: sessionID,
		from: *from, auditFrom: *auditFrom,
		format: *format, untilNow: *untilNow, interval: *interval,
		insecure: *insecure,
	}
	if err := run(ctx, opts); err != nil {
		fatal(err)
	}
}

type options struct {
	url       string
	token     string
	sessionID string
	from      int64
	auditFrom int64
	format    string
	untilNow  bool
	interval  time.Duration
	insecure  bool
}

func run(ctx context.Context, opts options) error {
	conn, hello, err := dial(ctx, opts)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	fmt.Fprintf(os.Stderr, "已连接: server=%s protocol=v%d\n", hello.ServerVersion, hello.ProtocolVersion)

	var wmu sync.Mutex
	send := func(typ string, payload any) error {
		env, err := protocol.New(typ, payload)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		return wsjson.Write(ctx, conn, env)
	}

	sessionID, err := pickSession(ctx, conn, send, opts.sessionID)
	if err != nil {
		return err
	}

	// 两个 head 都要取：-from/-audit-from 为负时用它们做「只看新数据」的起点，
	// -until-now 时用它们做导出终点（按启动时刻截断，之后产生的不追）。
	eventTarget, err := askEventHead(ctx, conn, send, sessionID)
	if err != nil {
		return err
	}
	eventHead := opts.from
	if eventHead < 0 {
		eventHead = eventTarget
	}
	if eventTarget < eventHead {
		eventTarget = eventHead
	}

	// 审计游标会被轮询协程与主循环同时读写，所以用原子量而不是裸 int64。
	auditTarget, err := askAuditHead(ctx, conn, send)
	if err != nil {
		return err
	}
	var auditCursor atomic.Int64
	if opts.auditFrom < 0 {
		auditCursor.Store(auditTarget)
	} else {
		auditCursor.Store(opts.auditFrom)
	}
	if auditTarget < auditCursor.Load() {
		auditTarget = auditCursor.Load()
	}

	if err := send(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: sessionID, FromSeq: eventHead,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "会话 %s，事件从 seq=%d 起，审计从 id=%d 起\n", sessionID, eventHead, auditCursor.Load())

	// 审计没有推送通道：按 id 游标轮询。事件是推送的，所以这里只需要一个
	// 轻量 ticker，失败一次不致命，下一轮继续。
	go func() {
		ticker := time.NewTicker(opts.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := send(protocol.TypeAuditTail, protocol.AuditTailPayload{AfterID: auditCursor.Load(), Limit: 200}); err != nil {
					return
				}
			}
		}
	}()
	if err := send(protocol.TypeAuditTail, protocol.AuditTailPayload{AfterID: auditCursor.Load(), Limit: 200}); err != nil {
		return err
	}

	out := &printer{format: opts.format}
	done := func(lastSeq int64) bool {
		return opts.untilNow && lastSeq >= eventTarget && auditCursor.Load() >= auditTarget
	}
	lastSeq := eventHead

	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("读事件失败: %w", err)
		}
		switch env.Type {
		case protocol.TypeEvent:
			var se protocol.StreamEventPayload
			if err := env.Decode(&se); err != nil {
				return err
			}
			out.event(env, se)
			if env.Seq > lastSeq {
				lastSeq = env.Seq
			}
		case protocol.TypeAuditTailResult:
			var p protocol.AuditTailResultPayload
			if err := env.Decode(&p); err != nil {
				return err
			}
			for _, r := range p.Records {
				out.audit(r)
			}
			if p.NextID > auditCursor.Load() {
				auditCursor.Store(p.NextID)
			}
		case protocol.TypeError:
			var p protocol.ErrorPayload
			_ = env.Decode(&p)
			return fmt.Errorf("服务端错误 %s: %s", p.Code, p.Message)
		default:
			// 快照、pong 等与导出无关。
		}
		if done(lastSeq) {
			return nil
		}
	}
}

func pickSession(ctx context.Context, conn *websocket.Conn, send func(string, any) error, want string) (string, error) {
	if want != "" {
		return want, nil
	}
	if err := send(protocol.TypeSessionList, nil); err != nil {
		return "", err
	}
	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return "", fmt.Errorf("读会话列表失败: %w", err)
		}
		switch env.Type {
		case protocol.TypeSessionListResult:
			var p protocol.SessionListResultPayload
			if err := env.Decode(&p); err != nil {
				return "", err
			}
			if len(p.Sessions) == 0 {
				return "", fmt.Errorf("还没有会话：先用 TUI 建一个，或用 -session 指定")
			}
			return p.Sessions[0].ID, nil
		case protocol.TypeError:
			var p protocol.ErrorPayload
			_ = env.Decode(&p)
			return "", fmt.Errorf("服务端错误 %s: %s", p.Code, p.Message)
		}
	}
}

func askEventHead(ctx context.Context, conn *websocket.Conn, send func(string, any) error, sessionID string) (int64, error) {
	if err := send(protocol.TypeEventHead, protocol.EventHeadPayload{SessionID: sessionID}); err != nil {
		return 0, err
	}
	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return 0, fmt.Errorf("读事件水位失败: %w", err)
		}
		switch env.Type {
		case protocol.TypeEventHeadResult:
			var p protocol.EventHeadResultPayload
			if err := env.Decode(&p); err != nil {
				return 0, err
			}
			return p.Seq, nil
		case protocol.TypeError:
			var p protocol.ErrorPayload
			_ = env.Decode(&p)
			return 0, fmt.Errorf("服务端错误 %s: %s", p.Code, p.Message)
		}
	}
}

func askAuditHead(ctx context.Context, conn *websocket.Conn, send func(string, any) error) (int64, error) {
	if err := send(protocol.TypeAuditTail, protocol.AuditTailPayload{AfterID: -1}); err != nil {
		return 0, err
	}
	for {
		var env protocol.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			return 0, fmt.Errorf("读审计水位失败: %w", err)
		}
		switch env.Type {
		case protocol.TypeAuditTailResult:
			var p protocol.AuditTailResultPayload
			if err := env.Decode(&p); err != nil {
				return 0, err
			}
			return p.NextID, nil
		case protocol.TypeError:
			var p protocol.ErrorPayload
			_ = env.Decode(&p)
			return 0, fmt.Errorf("服务端错误 %s: %s", p.Code, p.Message)
		}
	}
}

func dial(ctx context.Context, opts options) (*websocket.Conn, protocol.HelloPayload, error) {
	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + opts.token}},
	}
	if opts.insecure {
		dialOpts.HTTPClient = insecureHTTPClient()
	}
	conn, _, err := websocket.Dial(ctx, opts.url, dialOpts)
	if err != nil {
		return nil, protocol.HelloPayload{}, fmt.Errorf("连接 %s 失败: %w", opts.url, err)
	}
	conn.SetReadLimit(4 << 20)

	var hello protocol.Envelope
	if err := wsjson.Read(ctx, conn, &hello); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("读 hello 失败: %w", err)
	}
	if hello.Type != protocol.TypeHello {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("期待 hello，收到 %s", hello.Type)
	}
	var hp protocol.HelloPayload
	if err := hello.Decode(&hp); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, err
	}
	if hp.ProtocolVersion != protocol.Version {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("协议版本不匹配：服务端 %d，客户端 %d", hp.ProtocolVersion, protocol.Version)
	}
	env, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion: "jiaoag events", ProtocolVersion: protocol.Version,
	})
	if err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, err
	}
	if err := wsjson.Write(ctx, conn, env); err != nil {
		conn.CloseNow()
		return nil, protocol.HelloPayload{}, fmt.Errorf("发送 client.hello 失败: %w", err)
	}
	return conn, hp, nil
}

func insecureHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
}

// dialableAddr 与 TUI 一致：0.0.0.0 / 空主机名落到回环。
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
	fmt.Fprintf(os.Stderr, "jiaoag events: %v\n", err)
	os.Exit(1)
}

// printer 把事件/审计写成一行的 JSON 或人类可读文本。
//
// json 是默认：字段稳定、可直接 pipe 给 jq 或日志采集；text 只给人看，
// 长载荷会被截断，不适合做导出。
type printer struct {
	format string
}

func (p *printer) event(env protocol.Envelope, se protocol.StreamEventPayload) {
	if p.format == "text" {
		fmt.Printf("%s %-18s %8d  %s  %s\n",
			stamp(env.TS), se.Kind, env.Seq, se.TaskID, clip(se.Payload, 240))
		return
	}
	line := struct {
		Kind      string          `json:"kind"`
		TS        int64           `json:"ts"`
		SessionID string          `json:"session_id,omitempty"`
		Seq       int64           `json:"seq"`
		TaskID    string          `json:"task_id,omitempty"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload,omitempty"`
	}{"event", env.TS, env.SessionID, env.Seq, se.TaskID, se.Kind, se.Payload}
	writeJSON(line)
}

func (p *printer) audit(r protocol.AuditRecordPayload) {
	if p.format == "text" {
		fmt.Printf("%s %-18s %8d  %s  %-8s %s  %s\n",
			stamp(r.At), "audit:"+r.Action, r.ID, r.TaskID, r.Actor, r.ToolCallID, clip(r.Detail, 240))
		return
	}
	line := struct {
		Kind       string          `json:"kind"`
		ID         int64           `json:"id"`
		At         int64           `json:"at"`
		Actor      string          `json:"actor"`
		Action     string          `json:"action"`
		TaskID     string          `json:"task_id,omitempty"`
		SessionID  string          `json:"session_id,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		Detail     json.RawMessage `json:"detail,omitempty"`
	}{"audit", r.ID, r.At, r.Actor, r.Action, r.TaskID, r.SessionID, r.ToolCallID, r.Detail}
	writeJSON(line)
}

func writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "编码失败: %v\n", err)
		return
	}
	os.Stdout.Write(append(b, '\n'))
}

func stamp(ms int64) string {
	if ms == 0 {
		return "--:--:--"
	}
	return time.UnixMilli(ms).Format("15:04:05")
}

func clip(raw json.RawMessage, limit int) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len([]rune(s)) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}

// eventsHelp 说明这是只读导出：不审批、不改会话。
func eventsHelp(fs *flag.FlagSet) helppkg.Spec {
	return helppkg.Spec{
		Summary: "jiaoag events 把一条会话的事件与服务端审计导出成 JSONL（只读，不改会话）。",
		Usage:   []string{"jiaoag events [-s <会话 id>] [-from 0] [-audit-from 0] [--until-now]"},
		Flags:   fs,
		Examples: []string{
			"jiaoag events -s ses_3f2a19c4 -from 0 --until-now > events.jsonl",
			"jiaoag events -format text    # 人读的一行一条",
		},
	}
}
