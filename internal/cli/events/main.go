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
	"github.com/kennethfan/jiaoag/internal/store"
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
		dbPath    = flag.String("db", "", "不走服务端，直接读这个数据库文件（只读）")
		follow    = flag.Bool("follow", false, "持续跟随：断线自动重连，-output 文件被轮转后自动重开")
		output    = flag.String("output", "", "写入文件而不是 stdout（-follow 时追加，否则覆盖）")
	)
	var actions, types listFlag
	flag.Var(&actions, "action", "只导出 action 以该前缀开头的审计（可重复，如 -action exec -action write）")
	flag.Var(&types, "type", "只导出类型以该前缀开头的事件（可重复，如 -type approval -type tool）")
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
	if *follow && *untilNow {
		fmt.Fprintln(os.Stderr, "jiaoag events: -follow 与 -until-now 互斥（前者一直跟随，后者导到启动时刻就停）")
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 只看事件的命令不需要服务端：库是唯一的真相，直连读比走 WS 少一层
	// （不用 token、不用拉起进程，还能在服务端没跑的时候导出）。
	if strings.TrimSpace(*url) == "" {
		path := *dbPath
		if path == "" {
			path = cfg.Storage.DBPath
		}
		expanded, err := config.ExpandHome(path)
		if err != nil {
			fatal(err)
		}
		db, err := store.OpenReadOnly(ctx, expanded)
		if err != nil {
			fatal(err)
		}
		defer db.Close()
		opts := options{
			sessionID: sessionID,
			from:      *from, auditFrom: *auditFrom,
			format: *format, untilNow: *untilNow, interval: *interval,
			follow: *follow, output: *output,
			actions: []string(actions), types: []string(types),
		}
		if err := runLocal(ctx, db, expanded, opts); err != nil {
			fatal(err)
		}
		return
	}

	if err := cfg.ValidateClient(); err != nil {
		fatal(err)
	}
	if *token == "" {
		resolved, err := cfg.Server.ClientToken()
		if err != nil {
			fatal(err)
		}
		value, err := resolved.Resolve()
		if err != nil {
			fatal(fmt.Errorf("解析 server.token 失败: %w", err))
		}
		*token = value
	}

	target := *url

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
		follow:   *follow, output: *output,
		actions: []string(actions), types: []string(types),
	}
	if err := run(ctx, opts); err != nil {
		fatal(err)
	}
}

// listFlag 是可重复的字符串标志（-action exec -action write）。
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("不能为空")
	}
	*l = append(*l, v)
	return nil
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
	follow    bool
	output    string
	actions   []string
	types     []string
}

// resume 记录已经导出到哪里。断线重连时从这里续，避免重复行，也让
// -follow 在服务端重启后不漏掉中间的事件。
type resume struct {
	sessionID string
	lastSeq   int64
	cursor    int64
	started   bool
}

func run(ctx context.Context, opts options) error {
	out, err := newPrinter(opts)
	if err != nil {
		return err
	}
	defer out.Close()

	rs := &resume{}
	delay := 500 * time.Millisecond
	for {
		err := runWS(ctx, opts, out, rs)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if !opts.follow {
			return err
		}
		fmt.Fprintf(os.Stderr, "导出中断，%s 后重连：%v\n", delay, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}

func runWS(ctx context.Context, opts options, out *printer, rs *resume) error {
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

	sessionID := rs.sessionID
	if sessionID == "" {
		sessionID, err = pickSession(ctx, conn, send, opts.sessionID)
		if err != nil {
			return err
		}
		rs.sessionID = sessionID
	}

	// 两个 head 都要取：首次启动时用它们把「只看新数据」和「-until-now 的
	// 终点」定下来；重连时改用 rs 里的游标，head 只作为上限兜底。
	eventTarget, err := askEventHead(ctx, conn, send, sessionID)
	if err != nil {
		return err
	}
	auditTarget, err := askAuditHead(ctx, conn, send)
	if err != nil {
		return err
	}

	eventHead := rs.lastSeq
	if !rs.started {
		eventHead = opts.from
		if eventHead < 0 {
			eventHead = eventTarget
		}
	}
	if eventTarget < eventHead {
		eventTarget = eventHead
	}
	cursor := rs.cursor
	if !rs.started {
		cursor = opts.auditFrom
		if cursor < 0 {
			cursor = auditTarget
		}
	}
	if auditTarget < cursor {
		auditTarget = cursor
	}

	// 审计游标会被轮询协程与主循环同时读写，所以用原子量而不是裸 int64。
	var auditCursor atomic.Int64
	auditCursor.Store(cursor)
	rs.started = true

	if err := send(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: sessionID, FromSeq: eventHead,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "会话 %s，事件从 seq=%d 起，审计从 id=%d 起\n", sessionID, eventHead, cursor)

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

	done := func(lastSeq int64) bool {
		return opts.untilNow && lastSeq >= eventTarget && auditCursor.Load() >= auditTarget
	}
	lastSeq := eventHead
	rs.lastSeq = eventHead
	rs.cursor = cursor

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
				rs.lastSeq = env.Seq
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
				rs.cursor = p.NextID
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
	sink   sink
	filter eventFilter
	mu     sync.Mutex
}

// newPrinter 打开输出目标。sink 由调用方负责 Close（run 里 defer）。
func newPrinter(opts options) (*printer, error) {
	s, err := newSink(opts)
	if err != nil {
		return nil, err
	}
	return &printer{
		format: opts.format,
		sink:   s,
		filter: eventFilter{actions: opts.actions, types: opts.types},
	}, nil
}

func (p *printer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink == nil {
		return nil
	}
	return p.sink.Close()
}

// emit 串行化写入：审计轮询协程与主循环都会写同一个 sink。
func (p *printer) emit(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink == nil {
		// 测试里直接构造 printer 时不给 sink，退回 stdout。
		fmt.Fprintln(os.Stdout, line)
		return
	}
	if err := p.sink.writeLine([]byte(line)); err != nil {
		fmt.Fprintf(os.Stderr, "写出失败: %v\n", err)
	}
}

// eventFilter 是 -action / -type 的前缀过滤。空表示不过滤。
type eventFilter struct {
	actions []string
	types   []string
}

func (f eventFilter) allowsAction(action string) bool {
	return matchPrefix(f.actions, action)
}

func (f eventFilter) allowsEvent(kind string) bool {
	return matchPrefix(f.types, kind)
}

func matchPrefix(prefixes []string, s string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (p *printer) event(env protocol.Envelope, se protocol.StreamEventPayload) {
	if !p.filter.allowsEvent(se.Kind) {
		return
	}
	if p.format == "text" {
		p.emit(fmt.Sprintf("%s %-18s %8d  %s  %s",
			stamp(env.TS), se.Kind, env.Seq, se.TaskID, clip(se.Payload, 240)))
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
	p.writeJSON(line)
}

func (p *printer) audit(r protocol.AuditRecordPayload) {
	if !p.filter.allowsAction(r.Action) {
		return
	}
	if p.format == "text" {
		p.emit(fmt.Sprintf("%s %-18s %8d  %s  %-8s %s  %s",
			stamp(r.At), "audit:"+r.Action, r.ID, r.TaskID, r.Actor, r.ToolCallID, clip(r.Detail, 240)))
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
	p.writeJSON(line)
}

func (p *printer) writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "编码失败: %v\n", err)
		return
	}
	p.emit(string(b))
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
			"jiaoag events --follow -output ~/var/jiaoag/events.jsonl   # 持续跟随，日志轮转后自动重开",
			"jiaoag events --follow -action exec -action write          # 只看执行与写入的审计",
			"jiaoag events --follow -type approval -type tool           # 只看审批与工具事件",
		},
	}
}
