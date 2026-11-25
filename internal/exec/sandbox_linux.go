//go:build linux

package exec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Sandbox 用 bubblewrap 执行命令（ADR-0015）。
//
// 根文件系统只读挂载，再把工作区、临时目录与工具链缓存以可写方式 bind 回去；
// 敏感目录用 tmpfs 覆盖，命令看不到里面的内容。网络不隔离——用户明确选择
// 允许命令联网（拉依赖、跑测试都需要）。
type Sandbox struct {
	path    string
	version string
}

// NewSandbox 探测 bwrap 能否在当前内核上真正建立命名空间；失败返回
// ErrNotAvailable，由宿主决定报错，绝不静默回退 direct（ADR-0011）。
func NewSandbox() (*Sandbox, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: 找不到 bwrap", ErrNotAvailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, path,
		"--die-with-parent", "--new-session", "--unshare-pid",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: bwrap 探测失败（可能禁止非特权用户命名空间）: %v: %s", ErrNotAvailable, err, strings.TrimSpace(string(out)))
	}
	ver := "unknown"
	if out, err := exec.CommandContext(ctx, path, "--version").Output(); err == nil {
		ver = strings.TrimSpace(string(out))
	}
	return &Sandbox{path: path, version: "bwrap/" + ver}, nil
}

func (b *Sandbox) Name() string { return "sandbox" }

func (b *Sandbox) versionString() string { return b.version }

// Prepare 生成 bwrap 的最终 argv；Argv 仍保留用户命令，供审批展示。
func (b *Sandbox) Prepare(ctx context.Context, cmd Command) (Plan, error) {
	if err := validateCommand(ctx, cmd); err != nil {
		return Plan{}, err
	}
	paths := sandboxPolicyPaths(cmd)

	argv := []string{
		b.path,
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-uts",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	// 敏感目录优先覆盖：后面的可写 bind 不会把它们重新暴露出来，因为
	// sandboxPolicyPaths 已保证可写根不落在敏感目录内。
	for _, p := range paths.denied {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			argv = append(argv, "--tmpfs", p)
		} else {
			argv = append(argv, "--ro-bind", "/dev/null", p)
		}
	}
	for _, p := range paths.writable {
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			continue
		}
		argv = append(argv, "--bind", p, p)
	}
	argv = append(argv, "--chdir", cmd.Cwd, "--")
	argv = append(argv, cmd.Argv...)

	return Plan{
		Backend:      b.Name(),
		Version:      b.versionString(),
		Argv:         cmd.Argv,
		Cwd:          cmd.Cwd,
		Env:          cmd.Env,
		Timeout:      cmd.Timeout,
		Limits:       cmd.Limits,
		LimitSupport: supportLimits(),
		wrapped:      argv,
	}, nil
}

// Run 把资源限制套在 bwrap 外层：ulimit 会由子进程继承。
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
