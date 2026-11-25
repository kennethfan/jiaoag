//go:build darwin

package exec

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Sandbox 用 macOS 自带的 seatbelt（sandbox-exec）执行命令（ADR-0015）。
//
// 策略取保守的默认值：网络与只读访问照常，写入只允许工作区、临时目录与
// 工具链缓存；密钥目录禁止读取。它不是容器，也不试图防住同用户下的恶意
// 提权，只把「一条命令能顺手改坏多少东西」收窄到工作区。
type Sandbox struct {
	path string
}

// NewSandbox 探测 sandbox-exec 是否真的能跑；探测失败返回 ErrNotAvailable，
// 由宿主决定报错，绝不静默回退 direct（ADR-0011）。
func NewSandbox() (*Sandbox, error) {
	path, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, fmt.Errorf("%w: 找不到 sandbox-exec", ErrNotAvailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, path, "-p", "(version 1)(allow default)", "/usr/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: sandbox-exec 探测失败: %v: %s", ErrNotAvailable, err, strings.TrimSpace(string(out)))
	}
	return &Sandbox{path: path}, nil
}

func (b *Sandbox) Name() string { return "sandbox" }

func (b *Sandbox) version() string { return "sandbox/seatbelt-darwin" }

// Prepare 生成 sandbox-exec 的最终 argv；Argv 仍保留用户命令，供审批展示。
func (b *Sandbox) Prepare(ctx context.Context, cmd Command) (Plan, error) {
	if err := validateCommand(ctx, cmd); err != nil {
		return Plan{}, err
	}
	wrapped := make([]string, 0, len(cmd.Argv)+3)
	wrapped = append(wrapped, b.path, "-p", sandboxProfile(cmd))
	wrapped = append(wrapped, cmd.Argv...)
	return Plan{
		Backend:      b.Name(),
		Version:      b.version(),
		Argv:         cmd.Argv,
		Cwd:          cmd.Cwd,
		Env:          cmd.Env,
		Timeout:      cmd.Timeout,
		Limits:       cmd.Limits,
		LimitSupport: supportLimits(),
		wrapped:      wrapped,
	}, nil
}

// Run 把资源限制套在 sandbox-exec 外层：ulimit 会由子进程继承。
// Start 用后端包装后的 argv 非阻塞启动（sandbox 包装器留在前台，
// 被包装的命令因此仍处于沙箱内）。
func (b *Sandbox) Start(plan Plan, spec Spec) (*Process, error) {
	argv := plan.wrapped
	if len(argv) == 0 {
		argv = plan.Argv
	}
	return startBackground(plan, spec, argv)
}

func (b *Sandbox) Run(ctx context.Context, plan Plan, spec Spec) (Result, error) {
	return runPlan(ctx, plan, spec, wrapLimits(plan.wrapped, plan.Limits))
}

// sandboxProfile 生成 SBPL 文本。
//
// 顺序按「先放开默认、再收回写入、最后点名禁读」组织；deny 的敏感目录若包含
// 工作区会被 sandboxPolicyPaths 提前剔除，这里不再依赖规则优先级。
func sandboxProfile(cmd Command) string {
	paths := sandboxPolicyPaths(cmd)
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*")
	for _, p := range paths.writable {
		b.WriteString(" (subpath ")
		b.WriteString(strconv.Quote(p))
		b.WriteString(")")
	}
	// 命令经常把不需要的输出丢给 /dev/null；它不在任何可写根下。
	b.WriteString(" (literal \"/dev/null\")")
	b.WriteString(")\n")
	if len(paths.denied) > 0 {
		b.WriteString("(deny file-read*")
		for _, p := range paths.denied {
			b.WriteString(" (subpath ")
			b.WriteString(strconv.Quote(p))
			b.WriteString(")")
		}
		b.WriteString(")\n")
	}
	return b.String()
}
