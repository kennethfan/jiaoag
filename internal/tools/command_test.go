package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

type fakeBackend struct {
	result execpkg.Result
	err    error

	runs  int
	plans []execpkg.Plan
	specs []execpkg.Spec
}

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) Prepare(_ context.Context, cmd execpkg.Command) (execpkg.Plan, error) {
	return execpkg.Plan{Backend: "fake", Version: "fake/1", Argv: cmd.Argv, Cwd: cmd.Cwd, Env: cmd.Env, Timeout: cmd.Timeout}, nil
}

func (b *fakeBackend) Run(_ context.Context, plan execpkg.Plan, spec execpkg.Spec) (execpkg.Result, error) {
	b.runs++
	b.plans = append(b.plans, plan)
	b.specs = append(b.specs, spec)
	return b.result, b.err
}

func commandEnvFor(dir string, b execpkg.Backend) tools.RuntimeEnv {
	return tools.RuntimeEnv{Backend: b, OutputDir: dir, Timeout: 2 * time.Second, EventLimit: 8 << 10, LogLimit: 1 << 20}
}

func commandRegistry(t *testing.T, b execpkg.Backend, check bool) (*tools.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var tool tools.Tool
	if check {
		tool = tools.NewCheckCommand(ws, commandEnvFor(dir, b))
	} else {
		tool = tools.NewRunCommand(ws, commandEnvFor(dir, b))
	}
	reg, err := tools.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	return reg, dir
}

func call(name string, args string) tools.Call {
	return tools.Call{ID: "call_1", Name: name, Args: json.RawMessage(args)}
}

func TestRunCommandRendersOutputAndExitCode(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "ok\n", OutputPath: "/tmp/x.log"}}
	reg, dir := commandRegistry(t, b, false)

	res, p, err := reg.Run(context.Background(), call("run_command", `{"argv":["cat","a.txt"]}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError || !strings.Contains(res.Content, "ok") {
		t.Fatalf("结果 = %+v", res)
	}
	if res.OutputPath != "/tmp/x.log" || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("结果缺少退出码/日志路径：%+v", res)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Command == nil || p.Command.Cwd != root {
		t.Fatalf("cwd 应固定为工作区根 %s：%+v", root, p.Command)
	}
	if b.plans[0].Backend != "fake" {
		t.Fatalf("应通过注入的后端执行：%+v", b.plans[0])
	}
}

func TestRunCommandNonZeroExitIsToolError(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 2, Output: "boom\n"}}
	reg, _ := commandRegistry(t, b, false)
	res, _, err := reg.Run(context.Background(), call("run_command", `{"argv":["cat","missing"]}`))
	if err != nil {
		t.Fatalf("非零退出不应中断任务：%v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "退出码 2") {
		t.Fatalf("结果 = %+v", res)
	}
}

func TestCheckCommandHidesBodyOnSuccess(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "very long build log\n", OutputPath: "/tmp/x.log"}}
	reg, _ := commandRegistry(t, b, true)
	res, _, err := reg.Run(context.Background(), call("check_command", `{"argv":["go","build"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("成功不应是工具错误：%+v", res)
	}
	if strings.Contains(res.Content, "very long build log") {
		t.Fatalf("成功时不该带回正文：%q", res.Content)
	}
	if !strings.Contains(res.Content, "退出码 0") {
		t.Fatalf("应只回退出码：%q", res.Content)
	}
}

func TestCheckCommandIncludesTailOnFailure(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 1, Output: "compile error\n"}}
	reg, _ := commandRegistry(t, b, true)
	res, _, err := reg.Run(context.Background(), call("check_command", `{"argv":["go","build"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "compile error") {
		t.Fatalf("失败应带诊断：%+v", res)
	}
}

// TestCheckCommandKeepsFullTailViaCapture 走真实后端与 capture 路径：fakeBackend
// 直接给 Result，看不见 TailBytes 被压成事件预算 1/4 的问题。
func TestCheckCommandKeepsFullTailViaCapture(t *testing.T) {
	reg, _ := commandRegistry(t, execpkg.NewDirect(), true)
	// 先输出 6 KB，再把诊断放在最后并失败：尾部预算是 2 KB 时才留得住
	// 「一大段填充 + 末尾诊断」，只有 512 B 时填充会被压得过短。
	res, _, err := reg.Run(context.Background(), call("check_command",
		`{"argv":["/bin/sh","-c","yes x | head -c 6000; printf '\\nTAILDIAG-END'; exit 1"]}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError || res.ExitCode == nil || *res.ExitCode != 1 {
		t.Fatalf("结果 = %+v，期望退出码 1 的工具错误", res)
	}
	if !strings.Contains(res.Content, "TAILDIAG-END") {
		t.Fatalf("末尾诊断丢失：%q", res.Content)
	}
	if !res.Truncated {
		t.Fatal("6 KB 输出应标记截断")
	}
	// 2 KB 尾部 + 截断提示 + 退出码行；512 B 预算下只有约 550 字节。
	if len(res.Content) < 2000 {
		t.Fatalf("诊断只保留 %d 字节，尾部预算不是 2 KB：%q", len(res.Content), res.Content)
	}
}

// run_command 的载荷必须连同截断提示一起留在 8192 字节内：agent 只在
// > 8 KB 时才做「头 4 KB + 尾 2 KB」二次截断，超出这一档等于白留一份
// 永远到不了模型的中段。
func TestRunCommandPayloadStaysUnderAgentThreshold(t *testing.T) {
	reg, _ := commandRegistry(t, execpkg.NewDirect(), false)
	res, _, err := reg.Run(context.Background(), call("run_command",
		`{"argv":["/bin/sh","-c","yes x | head -c 200000"]}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated {
		t.Fatal("200 KB 输出应标记截断")
	}
	if len(res.Content) > 8<<10 {
		t.Fatalf("事件载荷 %d 字节，超过 agent 的 8 KB 阈值", len(res.Content))
	}
	if !strings.Contains(res.Content, "已截断") {
		t.Fatalf("载荷里应有截断提示：%q", res.Content[:64])
	}
}

// 日志写满不该把工具结果标成「事件输出已截断」，但必须让模型知道全文
// 后半段没落盘。
func TestCheckCommandLogTruncationIsWarningNotEventTruncation(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "ok\n", LogTruncated: true}}
	reg, _ := commandRegistry(t, b, true)
	res, _, err := reg.Run(context.Background(), call("check_command", `{"argv":["go","build"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Fatal("日志超限不应标记事件截断")
	}
	if !strings.Contains(res.Content, "全文日志上限") {
		t.Fatalf("应提示全文日志未写全：%q", res.Content)
	}
}

func TestCommandTimeoutIsToolError(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 124, TimedOut: true, Output: "partial"}}
	reg, _ := commandRegistry(t, b, false)
	res, _, err := reg.Run(context.Background(), call("run_command", `{"argv":["cat","a.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !res.TimedOut || !strings.Contains(res.Content, "超时") {
		t.Fatalf("结果 = %+v", res)
	}
}

func TestCommandRejectsInvalidArgv(t *testing.T) {
	reg, _ := commandRegistry(t, &fakeBackend{}, false)
	cases := []string{
		`{}`,
		`{"argv":[]}`,
		`{"argv":["cat",""]}`,
		`{"argv":["cat","a\u0000b"]}`,
		`{"argv":["cat"],"timeout_s":0}`,
		`{"argv":["cat"],"timeout_s":601}`,
	}
	for _, args := range cases {
		res, _, err := reg.Run(context.Background(), call("run_command", args))
		if err != nil {
			t.Fatalf("%s 不应中断任务：%v", args, err)
		}
		if !res.IsError {
			t.Fatalf("%s 应是工具错误，得到 %+v", args, res)
		}
	}

	long := strings.Repeat("a", 5<<10)
	res, _, _ := reg.Run(context.Background(), call("run_command", `{"argv":["cat","`+long+`"]}`))
	if !res.IsError {
		t.Fatal("超长 token 应被拒绝")
	}
}

func TestCommandStripsSecretEnv(t *testing.T) {
	t.Setenv("MY_SECRET_TOKEN", "s3cr3t")
	t.Setenv("OPENAI_API_KEY", "sk-x")
	reg, _ := commandRegistry(t, &fakeBackend{}, false)
	_, p, err := reg.Run(context.Background(), call("run_command", `{"argv":["cat","a.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	env := p.Command.Env
	for _, kv := range env {
		if strings.Contains(kv, "s3cr3t") || strings.Contains(kv, "sk-x") {
			t.Fatalf("密钥变量泄漏进子进程环境：%s", kv)
		}
	}
	if !slices.Contains(env, "PATH="+os.Getenv("PATH")) {
		t.Fatal("PATH 应保留，否则命令找不到可执行文件")
	}
}

func TestReadOutputPaginationAndBoundary(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ses", "tsk", "call.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := tools.RuntimeEnv{
		OutputDir: dir,
		SessionID: "ses",
		OutputPath: func(_ context.Context, id string) (string, error) {
			if id != "call_1" {
				t.Fatalf("意外 id：%s", id)
			}
			return logPath, nil
		},
	}
	reg, err := tools.NewRegistry(tools.NewReadOutput(env))
	if err != nil {
		t.Fatal(err)
	}

	res, _, err := reg.Run(context.Background(), call("read_output", `{"tool_call_id":"call_1","offset":2,"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "2: two") || strings.Contains(res.Content, "3: three") {
		t.Fatalf("分页结果不对：%q", res.Content)
	}
	if !res.Truncated {
		t.Fatal("后面还有行时应标记截断")
	}

	// 反查结果落在输出目录之外：即使是数据库里的路径也必须拒绝。
	env.OutputPath = func(context.Context, string) (string, error) {
		return filepath.Join(t.TempDir(), "elsewhere.log"), nil
	}
	outside, err := tools.NewRegistry(tools.NewReadOutput(env))
	if err != nil {
		t.Fatal(err)
	}
	res, _, err = outside.Run(context.Background(), call("read_output", `{"tool_call_id":"call_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "不在输出目录内") {
		t.Fatalf("越界路径应被拒绝：%+v", res)
	}

	// 另一个会话的输出即使落在同一输出根目录内也不能读。
	otherLog := filepath.Join(dir, "ses_other", "tsk", "call.log")
	if err := os.MkdirAll(filepath.Dir(otherLog), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherLog, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.OutputPath = func(context.Context, string) (string, error) { return otherLog, nil }
	crossSession, err := tools.NewRegistry(tools.NewReadOutput(env))
	if err != nil {
		t.Fatal(err)
	}
	res, _, err = crossSession.Run(context.Background(), call("read_output", `{"tool_call_id":"call_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "不在输出目录内") {
		t.Fatalf("跨会话读取应被拒绝：%+v", res)
	}
}

func TestGitToolsUseFixedArgv(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "abc commit\n"}}

	cases := []struct {
		tool tools.Tool
		args string
		want []string
	}{
		{tools.NewGitStatus(ws, commandEnvFor(dir, b)), `{}`, []string{
			"git", "status", "--porcelain=v1", "--untracked-files=normal"}},
		{tools.NewGitDiff(ws, commandEnvFor(dir, b)), `{}`, []string{
			"git", "diff", "--no-color", "--no-ext-diff", "--no-textconv"}},
		{tools.NewGitLog(ws, commandEnvFor(dir, b)), `{"max_lines":5}`, []string{
			"git", "log", "--oneline", "--no-color", "--no-ext-diff", "--no-textconv", "-n", "5"}},
	}
	for _, c := range cases {
		reg, err := tools.NewRegistry(c.tool)
		if err != nil {
			t.Fatal(err)
		}
		res, _, err := reg.Run(context.Background(), call(c.tool.Spec().Name, c.args))
		if err != nil {
			t.Fatalf("%s: %v", c.tool.Spec().Name, err)
		}
		if res.IsError {
			t.Fatalf("%s: %+v", c.tool.Spec().Name, res)
		}
		got := b.plans[len(b.plans)-1].Argv
		if !slices.Equal(got, c.want) {
			t.Fatalf("%s argv = %v，期望 %v", c.tool.Spec().Name, got, c.want)
		}
		// 仓库配置不能成为执行通道：每项 git 工具都必须带上 core.fsmonitor=false。
		if env := b.plans[len(b.plans)-1].Env; !slices.Contains(env, "GIT_CONFIG_KEY_0=core.fsmonitor") ||
			!slices.Contains(env, "GIT_CONFIG_VALUE_0=false") {
			t.Fatalf("%s 缺少 git 安全配置：%v", c.tool.Spec().Name, env)
		}
	}
}

func TestGitRefRejectsFlagLikeInput(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewGitDiff(ws, commandEnvFor(dir, &fakeBackend{})))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"-c", "--output=/tmp/x", "core.pager=sh"} {
		res, _, err := reg.Run(context.Background(), call("git_diff", `{"ref":"`+ref+`"}`))
		if err != nil {
			t.Fatalf("%s 不应中断任务：%v", ref, err)
		}
		if !res.IsError {
			t.Fatalf("ref %q 应被拒绝，得到 %+v", ref, res)
		}
	}
}

// git 工具也要把全文给后端：事件里只留 8 KB，中段必须能通过 read_output 补看。
func TestGitToolPassesLogPathAndEightKBudget(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "commit\n"}}
	reg, err := tools.NewRegistry(tools.NewGitLog(ws, commandEnvFor(dir, b)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Prepare(context.Background(), call("git_log", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "call.log")
	p.LogPath = logPath
	if _, err := reg.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	spec := b.specs[len(b.specs)-1]
	if spec.LogPath != logPath {
		t.Fatalf("LogPath = %q，期望 %q", spec.LogPath, logPath)
	}
	if spec.EventCap != 8<<10 {
		t.Fatalf("EventCap = %d，期望 %d", spec.EventCap, 8<<10)
	}
}

// 命令正文顶满事件预算时，加上「退出码 N」前缀不能越过 agent 的 8 KB 阈值：
// 一旦越过就会被二次折成首 4 KB + 尾 2 KB，白丢刚留好的尾部结论。
func TestRunCommandKeepsContentUnderAgentThreshold(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := commandEnvFor(dir, execpkg.NewDirect())
	reg, err := tools.NewRegistry(tools.NewRunCommand(ws, env))
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"argv":["/bin/sh","-c","printf HEADMARK; yes x | head -c 16000; printf TAILMARK; exit 1"]}`)
	res, _, err := reg.Run(context.Background(), tools.Call{ID: "call_1", Name: "run_command", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("退出码非 0 应标成错误：%+v", res)
	}
	if len(res.Content) > 8<<10 {
		t.Fatalf("结果 %d 字节，超过 agent 的 8 KB 二次截断阈值", len(res.Content))
	}
	for _, want := range []string{"HEADMARK", "TAILMARK", "已截断"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("结果缺少 %q：%q", want, res.Content)
		}
	}
}
