package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	gitStatusVersion = "1"
	gitDiffVersion   = "1"
	gitLogVersion    = "1"

	defaultGitLines = 30
	maxGitLines     = 200
	// gitEventCap 与命令工具对齐 8 KB：超过 agent 的 8 KB 阈值就会被二次截断，
	// 事件里多存的部分模型也看不到。
	gitEventCap = 8 << 10
	gitTimeout  = 30 * time.Second
)

// gitSafeEnv 是三项工具共同的安全配置（ADR-0004）。
//
// core.fsmonitor 会执行仓库配置里的钩子程序，`git status` 刷新索引时就会跑；
// diff.external / gitattributes 的 diff driver 会在 `git diff` 时执行外部命令。
// 仓库配置（.git/config）虽然写不进工作区（workspace.Resolve 拒绝 .git），
// 但一次获批的 `git config` 或旧的仓库配置就足以让「只读」变成任意执行。
//
// 用 GIT_CONFIG_* 环境变量注入而不是 `-c`：argv 保持「git <subcommand> ...」，
// 白名单与本地收紧规则按 subcommands 匹配时不需要知道内部注入了什么。
var gitSafeEnv = []string{
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=core.fsmonitor",
	"GIT_CONFIG_VALUE_0=false",
}

// gitSafeDiff 关闭外部 diff 与 textconv：两者都以仓库配置为命令来源。
var gitSafeDiff = []string{"--no-ext-diff", "--no-textconv"}

// gitRefPattern 只接受普通 ref / 路径形态，且必须以字母数字开头：
// 任何以 - 开头的参数都可能是 flag，透传出去就是绕过审批的入口。
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^-]*$`)

// NewGitStatus 返回只读 git status 工具。
func NewGitStatus(ws *workspace.Workspace, env RuntimeEnv) Tool {
	return &gitTool{ws: ws, env: env, mode: "status"}
}

// NewGitDiff 返回只读 git diff 工具。
func NewGitDiff(ws *workspace.Workspace, env RuntimeEnv) Tool {
	return &gitTool{ws: ws, env: env, mode: "diff"}
}

// NewGitLog 返回只读 git log 工具。
func NewGitLog(ws *workspace.Workspace, env RuntimeEnv) Tool {
	return &gitTool{ws: ws, env: env, mode: "log"}
}

// gitTool 内部拼固定 argv：模型只能填受约束的参数，不能传 -c / --output /
// --exec 之类的 flag（否则 `git -c core.pager='sh -c ...' log` 就是一条绕过
// 审批的任意执行通道）。
type gitTool struct {
	ws   *workspace.Workspace
	env  RuntimeEnv
	mode string
}

type gitStatusArgs struct {
	Path string `json:"path,omitempty"`
}

type gitDiffArgs struct {
	Path string `json:"path,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

type gitLogArgs struct {
	MaxLines int    `json:"max_lines,omitempty"`
	Path     string `json:"path,omitempty"`
}

func (t *gitTool) Spec() Spec {
	switch t.mode {
	case "status":
		return Spec{
			Name:        "git_status",
			Description: "查看工作区 git 状态（等价于 git status --porcelain）。只读，无需审批。",
			Version:     gitStatusVersion,
			Effects:     []Effect{EffectRead},
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "只看某个相对路径，默认整个工作区"}
  }
}`),
		}
	case "diff":
		return Spec{
			Name:        "git_diff",
			Description: "查看工作区 git diff（等价于 git diff --no-color）。只读，无需审批。",
			Version:     gitDiffVersion,
			Effects:     []Effect{EffectRead},
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "只看某个相对路径"},
    "ref":  {"type": "string", "description": "比较的 ref，例如 HEAD、HEAD~1、main"}
  }
}`),
		}
	default:
		return Spec{
			Name:        "git_log",
			Description: "查看提交历史（git log --oneline）。只读，无需审批。",
			Version:     gitLogVersion,
			Effects:     []Effect{EffectRead},
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "max_lines": {"type": "integer", "description": "最多返回多少条，默认 30，上限 200"},
    "path":      {"type": "string",  "description": "只看某个相对路径的历史"}
  }
}`),
		}
	}
}

func (t *gitTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	argv := []string{"git"}
	var paths []string
	switch t.mode {
	case "status":
		var args gitStatusArgs
		if err := decodeArgs(call, &args); err != nil {
			return Prepared{}, err
		}
		argv = append(argv, "status", "--porcelain=v1", "--untracked-files=normal")
		if args.Path != "" {
			abs, err := t.ws.Resolve(args.Path)
			if err != nil {
				return Prepared{}, err
			}
			argv = append(argv, "--", abs)
			paths = append(paths, abs)
		}
	case "diff":
		var args gitDiffArgs
		if err := decodeArgs(call, &args); err != nil {
			return Prepared{}, err
		}
		argv = append(argv, "diff", "--no-color")
		argv = append(argv, gitSafeDiff...)
		if args.Ref != "" {
			if !gitRefPattern.MatchString(args.Ref) {
				return Prepared{}, invalidArgs(call.Name, "ref %q 不合法：只接受字母数字开头的 ref", args.Ref)
			}
			argv = append(argv, args.Ref)
		}
		if args.Path != "" {
			abs, err := t.ws.Resolve(args.Path)
			if err != nil {
				return Prepared{}, err
			}
			argv = append(argv, "--", abs)
			paths = append(paths, abs)
		}
	case "log":
		var args gitLogArgs
		if err := decodeArgs(call, &args); err != nil {
			return Prepared{}, err
		}
		lines := args.MaxLines
		if lines <= 0 {
			lines = defaultGitLines
		}
		if lines > maxGitLines {
			lines = maxGitLines
		}
		argv = append(argv, "log", "--oneline", "--no-color")
		argv = append(argv, gitSafeDiff...)
		argv = append(argv, "-n", strconv.Itoa(lines))
		if args.Path != "" {
			abs, err := t.ws.Resolve(args.Path)
			if err != nil {
				return Prepared{}, err
			}
			argv = append(argv, "--", abs)
			paths = append(paths, abs)
		}
	default:
		return Prepared{}, fmt.Errorf("tools: 未知的 git 工具模式 %q", t.mode)
	}

	timeout := t.env.Timeout
	if timeout <= 0 {
		timeout = gitTimeout
	}
	if timeout > gitTimeout {
		timeout = gitTimeout
	}
	backend := ""
	if t.env.Backend != nil {
		backend = t.env.Backend.Name()
	}
	if len(paths) == 0 {
		paths = []string{t.ws.Root()}
	}
	return Prepared{
		Call:  call,
		Spec:  t.Spec(),
		Paths: paths,
		Command: &Command{
			Argv:    argv,
			Cwd:     t.ws.Root(),
			Timeout: timeout,
			Env:     append(commandEnv(os.Environ()), gitSafeEnv...),
			Backend: backend,
		},
	}, nil
}

// Execute 直接走执行后端。这些工具声明为只读，权限层不会为它们发起审批；
// argv 完全由本文件拼出，模型只填受约束的 path/ref/行数。
func (t *gitTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	cmd := p.Command
	if cmd == nil {
		return Result{}, fmt.Errorf("tools: %s 缺少命令", t.Spec().Name)
	}
	if t.env.Backend == nil {
		return Result{}, fmt.Errorf("tools: %s 没有可用的执行后端", t.Spec().Name)
	}
	started := time.Now()
	plan, err := t.env.Backend.Prepare(ctx, execpkg.Command{
		Argv:    cmd.Argv,
		Cwd:     cmd.Cwd,
		Timeout: cmd.Timeout,
		Env:     cmd.Env,
	})
	if err != nil {
		return Result{}, err
	}
	res, err := t.env.Backend.Run(ctx, plan, execpkg.SpecFor(p.LogPath, gitEventCap, t.env.LogLimit))
	if err != nil {
		return Result{}, err
	}
	code := res.ExitCode
	out := Result{
		ExitCode:   &code,
		TimedOut:   res.TimedOut,
		Truncated:  res.Truncated,
		OutputPath: res.OutputPath,
		LogError:   res.LogError,
	}
	out.Elapsed = time.Since(started)
	out.BackendVersion = plan.Version
	out.Content = strings.TrimRight(res.Output, "\n")
	if out.Content == "" {
		out.Content = "（无输出）"
	}
	if res.LogError != "" {
		// 日志没写成不能假装「全文已落盘」：模型会照着 output_path 去读。
		out.OutputPath = ""
		out.Content += "\n（警告：全文日志写入失败：" + clipLogError(res.LogError) + "）"
	}
	if res.LogTruncated {
		out.Content += "\n（警告：输出超过全文日志上限，后半段未写入日志）"
	}
	if res.TimedOut {
		out.IsError = true
		out.Content = "git 命令超时被终止"
	} else if res.ExitCode != 0 {
		out.IsError = true
		out.Content = fmt.Sprintf("退出码 %d\n%s", res.ExitCode, out.Content)
	}
	return out, nil
}
