package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	runCommandVersion   = "1"
	checkCommandVersion = "1"

	defaultCommandTimeout = 120 * time.Second
	minCommandTimeout     = time.Second
	maxCommandTimeout     = 600 * time.Second

	maxArgvTokens = 128
	maxTokenBytes = 4 << 10

	// commandEventReserve 是事件载荷给「退出码 N」「日志警告」留的余量。
	//
	// agent 在 8 KB 处会做二次截断（首 4 KB + 尾 2 KB）：run_command 的正文
	// 连同这些附加文本必须留在 8192 字节内，否则刚留好的尾部结论会被截掉。
	commandEventReserve = 200
)

// Command 是一次待执行的命令。Argv 逐 token 传递，永远不经 shell。
type Command struct {
	Argv    []string
	Cwd     string
	Timeout time.Duration
	Env     []string
	// Backend 由宿主注入的工具环境填写，仅供审批面板与审计展示；
	// 模型无权指定（ADR-0011）。
	Backend string
}

// RuntimeEnv 是命令类工具的宿主依赖。工具不自己构造执行后端，也不自己
// 决定输出目录：两者都由 app 注入。
type RuntimeEnv struct {
	Backend   execpkg.Backend
	OutputDir string
	// SessionID 把 read_output 的读取范围钉在本会话的输出子树内。
	SessionID  string
	Timeout    time.Duration
	EventLimit int
	LogLimit   int
	// Limits 是单条命令的资源上限，由宿主配置注入；工具自己不做判定。
	Limits execpkg.Limits
	// OutputPath 把 tool_call_id 反查成全文日志路径；read_output 用它，
	// 模型不能直接给路径。
	OutputPath func(ctx context.Context, toolCallID string) (string, error)
}

// NewRunCommand 返回带回正文的命令工具。
func NewRunCommand(ws *workspace.Workspace, env RuntimeEnv) Tool {
	return &commandTool{ws: ws, env: env, capture: true}
}

// NewCheckCommand 返回只关心退出码的命令工具：成功时不带正文，失败时带尾部
// 诊断。拆成两个工具是让「省上下文」成为模型的一次显式选择。
func NewCheckCommand(ws *workspace.Workspace, env RuntimeEnv) Tool {
	return &commandTool{ws: ws, env: env, capture: false}
}

type commandTool struct {
	ws      *workspace.Workspace
	env     RuntimeEnv
	capture bool
}

type commandArgs struct {
	Argv     []string `json:"argv"`
	TimeoutS *int     `json:"timeout_s,omitempty"`
}

func (t *commandTool) name() string {
	if t.capture {
		return "run_command"
	}
	return "check_command"
}

func (t *commandTool) Spec() Spec {
	name := t.name()
	version := runCommandVersion
	desc := "执行一条命令并带回合并后的 stdout/stderr（最多 8 KB，保留头尾；全文写入日志）。" +
		"argv 逐 token 传递，不经过 shell；不会自动放行的命令需要用户批准。"
	schema := `{
  "type": "object",
  "properties": {
    "argv":      {"type": "array", "items": {"type": "string"}, "description": "逐 token 的 argv，例如 [\"go\",\"test\",\"./...\"]；不要传 shell 字符串"},
    "timeout_s": {"type": "integer", "description": "超时秒数，默认 120，范围 1-600"}
  },
  "required": ["argv"]
}`
	if !t.capture {
		version = checkCommandVersion
		desc = "执行一条命令，只带回退出码；失败时额外带回尾部诊断（最多 2 KB）。" +
			"适合 go build / go vet / git status 这类只关心成败的命令。"
	}
	return Spec{
		Name:        name,
		Description: desc,
		Version:     version,
		Effects:     []Effect{EffectProcess},
		Schema:      json.RawMessage(schema),
	}
}

func (t *commandTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	var args commandArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	if len(args.Argv) == 0 {
		return Prepared{}, invalidArgs(call.Name, "argv 不能为空")
	}
	if len(args.Argv) > maxArgvTokens {
		return Prepared{}, invalidArgs(call.Name, "argv 有 %d 个 token，超过上限 %d", len(args.Argv), maxArgvTokens)
	}
	for i, token := range args.Argv {
		if token == "" {
			return Prepared{}, invalidArgs(call.Name, "argv[%d] 为空", i)
		}
		if len(token) > maxTokenBytes {
			return Prepared{}, invalidArgs(call.Name, "argv[%d] 有 %d 字节，超过上限 %d", i, len(token), maxTokenBytes)
		}
		if strings.ContainsRune(token, 0) {
			return Prepared{}, invalidArgs(call.Name, "argv[%d] 含 NUL 字节", i)
		}
	}

	timeout := t.env.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	if args.TimeoutS != nil {
		seconds := *args.TimeoutS
		if seconds < 1 || seconds > 600 {
			return Prepared{}, invalidArgs(call.Name, "timeout_s 必须在 1-600 之间，收到 %d", seconds)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	timeout = clampTimeout(timeout)

	backend := ""
	if t.env.Backend != nil {
		backend = t.env.Backend.Name()
	}
	argv := make([]string, len(args.Argv))
	copy(argv, args.Argv)

	cmd := &Command{
		Argv:    argv,
		Cwd:     t.ws.Root(),
		Timeout: timeout,
		Env:     commandEnv(os.Environ()),
		Backend: backend,
	}
	return Prepared{
		Call:    call,
		Spec:    t.Spec(),
		Args:    args,
		Paths:   []string{cmd.Cwd},
		Command: cmd,
	}, nil
}

func (t *commandTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	cmd := p.Command
	if cmd == nil {
		return Result{}, fmt.Errorf("tools: %s 缺少命令", t.name())
	}
	if t.env.Backend == nil {
		return Result{}, fmt.Errorf("tools: %s 没有可用的执行后端", t.name())
	}

	plan, err := t.env.Backend.Prepare(ctx, execpkg.Command{
		Argv:    cmd.Argv,
		Cwd:     cmd.Cwd,
		Timeout: cmd.Timeout,
		Env:     cmd.Env,
		Limits:  t.env.Limits,
	})
	if err != nil {
		return Result{}, err
	}

	started := time.Now()
	res, err := t.env.Backend.Run(ctx, plan, t.spec(p.LogPath))
	if err != nil {
		return Result{}, err
	}
	out := t.render(res, cmd.Timeout)
	out.Elapsed = time.Since(started)
	out.BackendVersion = plan.Version
	out.Limits = plan.Limits
	out.LimitsDegraded = plan.LimitsDegraded()
	return out, nil
}

func (t *commandTool) spec(logPath string) execpkg.Spec {
	eventCap := t.env.EventLimit
	if eventCap <= 0 {
		eventCap = 8 << 10
	}
	if !t.capture {
		// check_command 的 2 KB 天然低于 agent 的 8 KB 截断阈值：模型看到的
		// 就是工具返回的全部，不会再被二次截断。
		eventCap = 2 << 10
	} else if eventCap > commandEventReserve {
		// 扣掉「退出码 N」与日志警告的余量：正文顶满 8 KB 再加前缀，
		// agent 就会把它折成 4 KB + 2 KB，白丢一段。
		eventCap -= commandEventReserve
	}
	spec := execpkg.SpecFor(logPath, eventCap, t.env.LogLimit)
	if !t.capture {
		// check_command 只要尾部诊断：编译错误几乎总在末尾，头部只是进度。
		// 诊断取合并流的末尾，不按 stderr 优先：go build 的错误多在 stderr、
		// go test 的失败细节在 stdout，按流挑选会把另一类内容整段丢掉。
		spec.TailOnly = true
		// SpecFor 的 1/4 尾部预算是为「头 + 尾」两段留的；tail-only 没有
		// 头段，整个事件预算都该给尾部，否则 2 KB 诊断实际只剩 512 B。
		spec.TailBytes = eventCap
	}
	return spec
}

// render 在正文之后补日志警告：没有全文时不能假装输出已落盘。
func (t *commandTool) render(res execpkg.Result, timeout time.Duration) Result {
	out := t.renderBody(res, timeout)
	if res.LogError != "" {
		// 错误文案本身也可能很长（系统错误会带路径），先裁短再拼，
		// 否则这条警告就能把 8 KB 预算撑破。
		out.LogError = clipLogError(res.LogError)
		out.Content += "\n（警告：全文日志写入失败：" + out.LogError + "）"
		// 日志没写成，output_path 不能留给模型一个读不到/读不全的文件。
		out.OutputPath = ""
	}
	if res.LogTruncated {
		out.Content += "\n（警告：输出超过全文日志上限，后半段未写入日志）"
	}
	return out
}

func (t *commandTool) renderBody(res execpkg.Result, timeout time.Duration) Result {
	code := res.ExitCode
	out := Result{
		ExitCode:   &code,
		OutputPath: res.OutputPath,
		Truncated:  res.Truncated,
		TimedOut:   res.TimedOut,
	}
	if res.TimedOut {
		out.IsError = true
		out.Content = fmt.Sprintf("命令超时被终止（%s）", timeout)
		if body := strings.TrimRight(res.Output, "\n"); body != "" {
			out.Content += "\n" + body
		}
		return out
	}

	if t.capture {
		out.Content = res.Output
		if strings.TrimSpace(out.Content) == "" {
			out.Content = "（无输出）"
		}
		if res.ExitCode != 0 {
			out.IsError = true
			out.Content = fmt.Sprintf("退出码 %d\n%s", res.ExitCode, out.Content)
		}
		return out
	}

	if res.ExitCode == 0 {
		// 成功时不带正文：正文只在日志文件里，需要时用 read_output 取。
		out.Content = "退出码 0"
		if res.OutputPath != "" {
			out.Content += "（全文：" + res.OutputPath + "）"
		}
		return out
	}
	out.IsError = true
	body := strings.TrimRight(res.Output, "\n")
	if body == "" {
		out.Content = fmt.Sprintf("退出码 %d（无诊断输出）", res.ExitCode)
		return out
	}
	out.Content = fmt.Sprintf("退出码 %d\n%s", res.ExitCode, body)
	return out
}

// clipLogError 把日志错误压成单行短文本：它会进 tool.result 与审计 detail，
// 不能带着完整的系统错误（含长路径）把预算撑破。
func clipLogError(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	// 按字节裁：调用方按字节预算（8 KB 事件载荷），按 rune 裁会让中文错误
	// 占到三倍预算。裁口回退到 UTF-8 边界，避免切坏字符。
	const max = 100
	if len(msg) <= max {
		return msg
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func clampTimeout(d time.Duration) time.Duration {
	if d < minCommandTimeout {
		return minCommandTimeout
	}
	if d > maxCommandTimeout {
		return maxCommandTimeout
	}
	return d
}

// commandEnv 剥离密钥类变量，只保留运行命令所需的基础项。
//
// 白名单而不是黑名单：DATABASE_URL 这类变量也可能内嵌口令，而黑名单永远
// 漏得掉。代价是某些自定义工具链变量要显式加进来。
func commandEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !envAllowed(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

var execEnvAllow = map[string]bool{
	"PATH": true, "HOME": true, "LANG": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"TZ": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true,
	"GOPATH": true, "GOCACHE": true, "GOMODCACHE": true, "GOROOT": true,
	"GOFLAGS": true, "GOTOOLCHAIN": true, "GONOSUMDB": true, "GOPRIVATE": true,
	"GONOSUMCHECK": true, "GOSUMDB": true, "SystemRoot": true, "PATHEXT": true,
}

var sensitiveEnvParts = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"}

func envAllowed(key string) bool {
	upper := strings.ToUpper(key)
	for _, part := range sensitiveEnvParts {
		if strings.Contains(upper, part) {
			return false
		}
	}
	if execEnvAllow[key] {
		return true
	}
	return strings.HasPrefix(key, "LC_")
}
