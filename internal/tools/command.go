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
	"github.com/kennethfan/jiaoag/internal/project"
	"github.com/kennethfan/jiaoag/internal/skills"
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
	// maxShellBytes 限制整条 shell 命令。shell 文本是一条整体，不按 token
	// 逐个校验：一条带内联脚本的命令动辄上万字节，而 maxTokenBytes 是给
	// 单个 argv 元素设的。
	maxShellBytes = 8 << 10

	// commandEventReserve 是事件载荷给「退出码 N」「日志警告」留的余量。
	//
	// agent 在 8 KB 处会做二次截断（首 4 KB + 尾 2 KB）：run_command 的正文
	// 连同这些附加文本必须留在 8192 字节内，否则刚留好的尾部结论会被截掉。
	commandEventReserve = 200
)

// Command 是一次待执行的命令。模型可以直接给 argv（逐 token，不经 shell，
// 可能命中命令白名单），也可以给一整条 shell 命令由系统 shell 解释。
type Command struct {
	Argv    []string
	Cwd     string
	Timeout time.Duration
	Env     []string
	// Shell 非空表示这次调用请求了 shell 解释：Argv 已是 [shell, -c, Shell]
	// 的形态。审批面板据此显示整条命令而不是拆成三段；策略层据此跳过
	// 元字符判定——元字符对 shell 是正常语法，对 argv 才可疑。
	Shell string
	// Backend 由宿主注入的工具环境填写，仅供审批面板与审计展示；
	// 模型无权指定（ADR-0011）。
	Backend string
	// Root 是命令所在根的别名（空表示主根），同样只用于展示与审计。
	Root string
	// Writable 是本次命令可写的根（主根 + 附加根），交给隔离后端算权限；
	// direct 后端不读它。
	Writable []string
	// Background 表示这次要非阻塞启动：启动成功即返回，进程登记在
	// ProcTable 里，输出持续写日志，直到 stop_command 或服务端关闭。
	Background bool
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
	// Skills 是本次任务的技能库；非空时注册 skill 工具。
	Skills *skills.Catalog
	// ProjectServers 是工作区里已确认的 MCP server（ADR-0025）；由宿主
	// 转换成 mcp.Server 后与全局配置一起启动。
	ProjectServers []project.Server
	// Roots 是会话挂载的根集合（ADR-0036/ADR-0037）；nil 表示只有主根。
	Roots *workspace.Roots
	// Procs 登记后台进程（run_command background=true），按会话隔离。
	// nil 表示宿主未启用后台命令，工具会明确报错而不是假装启动。
	Procs *execpkg.ProcTable
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
	Argv []string `json:"argv"`
	// Shell 是整条由系统 shell 解释的命令，与 Argv 二选一。
	Shell    string `json:"shell,omitempty"`
	TimeoutS *int   `json:"timeout_s,omitempty"`
	// Background 把命令放到后台跑：立即返回 id，输出写日志，用
	// stop_command 停止。与 timeout_s 互斥——后台进程本来就不该被超时杀掉。
	Background bool `json:"background,omitempty"`
	// Root 指定在哪个附加根下执行；省略即主根。
	Root string `json:"root,omitempty"`
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
		"能在 argv 里逐 token 表达时用 argv（不经 shell，可能被命令白名单自动放行）。" +
		"需要管道、重定向、通配符或多条命令时用 shell 字段写整条命令，由系统 shell 解释，" +
		"这条一定需要用户批准；不要把整条命令塞进 argv（例如 [\"sh\",\"-c\",\"…\"]），" +
		"那样会被当成普通程序调用，审批时也看不出你的意图。"
	schema := `{
  "type": "object",
  "properties": {
    "argv":      {"type": "array", "items": {"type": "string"}, "description": "逐 token 的 argv，例如 [\"go\",\"test\",\"./...\"]；不要写成 [\"sh\",\"-c\",\"…\"]"},
    "shell":     {"type": "string", "description": "需要管道、重定向、通配符或多条命令时写整条命令，例如 \"go test ./... 2>&1 | tail -20\"；与 argv 二选一，且一定要用户批准"},
    "timeout_s": {"type": "integer", "description": "超时秒数，默认 120，范围 1-600；与 background 互斥"},
    "background": {"type": "boolean", "description": "放到后台跑（dev server、监听进程）：立即返回 id，输出写日志，用 read_output 查看、stop_command 停止"},
    "root":      {"type": "string", "description": "在哪个附加根下执行（别名）；省略表示主根"}
  },
  "anyOf": [{"required": ["argv"]}, {"required": ["shell"]}]
}`
	if !t.capture {
		version = checkCommandVersion
		desc = "执行一条命令，只带回退出码；失败时额外带回尾部诊断（最多 2 KB）。" +
			"适合 go build / go vet / git status 这类只关心成败的命令。" +
			"需要管道或多条命令时用 shell 字段写整条，不要塞进 argv（那就成了 sh -c 的普通程序调用）。"
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
	shell := strings.TrimSpace(args.Shell)
	switch {
	case len(args.Argv) == 0 && shell == "":
		return Prepared{}, invalidArgs(call.Name, "argv 与 shell 至少要给一个")
	case len(args.Argv) > 0 && shell != "":
		// 二选一是刻意的：一条命令要么能被拆成 argv（可静态判定、可走白名单），
		// 要么整体交给 shell（一定要审批）。同时给会让「到底跑了什么」变得含糊。
		return Prepared{}, invalidArgs(call.Name, "argv 与 shell 只能二选一：能拆成 argv 时不要用 shell")
	}

	var rawArgv []string
	if shell != "" {
		if len(shell) > maxShellBytes {
			return Prepared{}, invalidArgs(call.Name, "shell 命令有 %d 字节，超过上限 %d", len(shell), maxShellBytes)
		}
		if strings.ContainsRune(shell, 0) {
			return Prepared{}, invalidArgs(call.Name, "shell 命令含 NUL 字节")
		}
		rawArgv = shellArgv(shell)
	} else {
		rawArgv = args.Argv
		if len(rawArgv) > maxArgvTokens {
			return Prepared{}, invalidArgs(call.Name, "argv 有 %d 个 token，超过上限 %d", len(rawArgv), maxArgvTokens)
		}
		for i, token := range rawArgv {
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
	}

	if args.Background {
		if !t.capture {
			return Prepared{}, invalidArgs(call.Name, "check_command 不支持 background")
		}
		if args.TimeoutS != nil {
			return Prepared{}, invalidArgs(call.Name, "background 与 timeout_s 只能二选一：后台进程不设超时")
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
	argv := make([]string, len(rawArgv))
	copy(argv, rawArgv)

	// 命令的 cwd 决定 path_args 的判定边界：在附加根里跑就在那个根里判定，
	// 不能拿主根去卡另一个仓的路径。
	target := t.ws
	rootAlias := ""
	if alias := strings.TrimSpace(args.Root); alias != "" {
		if t.env.Roots == nil {
			return Prepared{}, invalidArgs(call.Name, "会话没有挂载附加根，root=%q 无法使用", alias)
		}
		ws, ok := t.env.Roots.Get(alias)
		if !ok {
			return Prepared{}, invalidArgs(call.Name, "%v：%q", workspace.ErrUnknownRoot, alias)
		}
		target = ws
		rootAlias = alias
	}

	cmd := &Command{
		Argv:       argv,
		Shell:      shell,
		Cwd:        target.Root(),
		Timeout:    timeout,
		Env:        commandEnv(os.Environ()),
		Backend:    backend,
		Root:       rootAlias,
		Writable:   writableRoots(t.ws, t.env.Roots),
		Background: args.Background,
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
		Argv:     cmd.Argv,
		Cwd:      cmd.Cwd,
		Timeout:  cmd.Timeout,
		Env:      cmd.Env,
		Limits:   t.env.Limits,
		Writable: cmd.Writable,
	})
	if err != nil {
		return Result{}, err
	}

	if cmd.Background {
		return t.startBackground(ctx, p, plan)
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

// writableRoots 返回会话可写的根目录（主根 + 附加根），给隔离后端用。
func writableRoots(primary *workspace.Workspace, roots *workspace.Roots) []string {
	out := []string{primary.Root()}
	if roots == nil {
		return out
	}
	for _, alias := range roots.Aliases() {
		if ws, ok := roots.Get(alias); ok {
			out = append(out, ws.Root())
		}
	}
	return out
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
// CommandEnv 是命令类工具共用的环境变量策略：白名单放行、剥离密钥。
//
// 开放给插件宿主使用，避免插件命令继承父进程的全部环境（那就等于绕过了
// 「密钥不进子进程」这条约束）。
func CommandEnv(environ []string) []string { return commandEnv(environ) }

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

// execEnvAllow 是允许传给子进程的变量。键统一大写：Windows 的环境变量名
// 大小写不敏感，父进程给的常常是 "Path" 这种写法，按原样查表会把 PATH 整条
// 丢掉，子进程连命令都找不到。
var execEnvAllow = map[string]bool{
	"PATH": true, "HOME": true, "LANG": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"TZ": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true,
	"GOPATH": true, "GOCACHE": true, "GOMODCACHE": true, "GOROOT": true,
	"GOFLAGS": true, "GOTOOLCHAIN": true, "GONOSUMDB": true, "GOPRIVATE": true,
	"GONOSUMCHECK": true, "GOSUMDB": true,
	// Windows 上缺了这些，cmd / powershell 找不到解释器与自己的模块：
	// 没有 USERPROFILE，go 连构建缓存目录都定不下来。
	"SYSTEMROOT": true, "WINDIR": true, "SYSTEMDRIVE": true, "PATHEXT": true,
	"COMSPEC": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"PROGRAMDATA": true, "PROGRAMFILES": true, "PSMODULEPATH": true,
	"NUMBER_OF_PROCESSORS": true, "PROCESSOR_ARCHITECTURE": true,
}

var sensitiveEnvParts = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"}

func envAllowed(key string) bool {
	upper := strings.ToUpper(key)
	for _, part := range sensitiveEnvParts {
		if strings.Contains(upper, part) {
			return false
		}
	}
	if execEnvAllow[upper] {
		return true
	}
	return strings.HasPrefix(upper, "LC_")
}

// startBackground 启动一条后台命令并登记到会话的进程表。
//
// 返回的 tool_call_id 就是后台 id：模型不需要另记一个标识，read_output 与
// stop_command 都用它，事件与审计里也能对回同一行工具调用。
func (t *commandTool) startBackground(ctx context.Context, p Prepared, plan execpkg.Plan) (Result, error) {
	cmd := p.Command
	if t.env.Procs == nil {
		return Result{}, fmt.Errorf("tools: 宿主未启用后台命令")
	}
	bg, ok := t.env.Backend.(execpkg.Background)
	if !ok {
		return Result{}, fmt.Errorf("tools: 执行后端 %s 不支持后台命令", t.env.Backend.Name())
	}
	if p.LogPath == "" {
		return Result{}, fmt.Errorf("tools: 后台命令缺少日志路径")
	}
	proc, err := bg.Start(plan, t.spec(p.LogPath))
	if err != nil {
		return Result{}, err
	}
	proc.ID = p.Call.ID
	if err := t.env.Procs.Add(t.env.SessionID, proc); err != nil {
		_ = proc.Stop()
		return Result{}, err
	}
	command := strings.Join(cmd.Argv, " ")
	content := fmt.Sprintf("已启动后台命令（id %s）：%s\n"+
		"输出持续写入日志：用 read_output 传 tool_call_id=%s 查看；"+
		"需要停止时调用 stop_command（同样传这个 id）。", proc.ID, command, proc.ID)
	return Result{
		Content:        content,
		OutputPath:     proc.OutputPath,
		BackendVersion: plan.Version,
	}, nil
}
