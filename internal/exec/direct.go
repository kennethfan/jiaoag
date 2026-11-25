package exec

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// Direct 是 v1 唯一的执行后端：直接在宿主上跑（ADR-0011）。
//
// 它不是安全边界——命令是否可执行由 internal/perm 判定，这里只负责
// 起进程、收敛输出、按超时与取消杀进程组。
type Direct struct{}

// NewDirect 返回宿主直连后端。
func NewDirect() *Direct { return &Direct{} }

func (b *Direct) Name() string { return "direct" }

func (b *Direct) version() string { return "direct/" + runtime.GOOS }

// Prepare 校验命令形状并返回最终计划，不执行任何东西。
func (b *Direct) Prepare(ctx context.Context, cmd Command) (Plan, error) {
	if err := validateCommand(ctx, cmd); err != nil {
		return Plan{}, err
	}
	return Plan{
		Backend:      b.Name(),
		Version:      b.version(),
		Argv:         cmd.Argv,
		Cwd:          cmd.Cwd,
		Env:          cmd.Env,
		Timeout:      cmd.Timeout,
		Limits:       cmd.Limits,
		LimitSupport: supportLimits(),
	}, nil
}

// Run 执行计划。ctx 取消返回 ctx.Err()，超时返回带 TimedOut 的结果。
func (b *Direct) Run(ctx context.Context, plan Plan, spec Spec) (Result, error) {
	// 资源限制通过 shell 包装施加；Plan.Argv 保持原始形状供审批与审计展示。
	return runPlan(ctx, plan, spec, wrapLimits(plan.Argv, plan.Limits))
}

// runPlan 是各后端共用的执行循环：起进程、收敛输出、按超时与取消杀进程组。
// argv 已经是最终要交给内核的形状（后端包装 + 资源限制包装）。
// Start 非阻塞启动一条命令并返回句柄。输出持续写入 spec.LogPath，
// 生命周期由调用方（ProcTable）负责。
func (b *Direct) Start(plan Plan, spec Spec) (*Process, error) {
	return startBackground(plan, spec, plan.Argv)
}

func runPlan(ctx context.Context, plan Plan, spec Spec, argv []string) (Result, error) {
	started := time.Now()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	capture := newCapture(spec)
	// 先组装结果再关文件：日志路径与 total 都要在关闭前算好。
	var res Result
	defer func() { capture.Close() }()

	runCtx := ctx
	if plan.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, plan.Timeout)
		defer cancel()
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = plan.Cwd
	// nil 表示沿用父进程环境；显式空切片表示「清洗后没有可保留的变量」。
	// 命令工具总是给出非 nil 的 Env，因此密钥类变量不会被静默继承。
	if plan.Env != nil {
		cmd.Env = plan.Env
	}
	// stdout / stderr 合并进同一个 capture：check_command 的尾部诊断取合并流，
	// 不按流优先——go build 的错误多在 stderr、go test 的失败细节在 stdout，
	// 挑一个流会丢掉另一类最有用的一段。合并也保留了两者的时序。
	cmd.Stdout = capture
	cmd.Stderr = capture

	pg, err := newProcGroup(plan.Limits)
	if err != nil {
		return Result{}, wrap(plan, err)
	}
	// LIFO：先关进程组（Windows 的 KILL_ON_JOB_CLOSE 会收掉残留进程），
	// 再关 capture，避免还有进程往已经关闭的日志句柄里写。
	defer pg.close()
	pg.beforeStart(cmd)

	if err := cmd.Start(); err != nil {
		return Result{}, wrap(plan, err)
	}
	if err := pg.afterStart(cmd); err != nil {
		// 限制没能施加到这条命令上，就按失败处理：杀掉并如实报错，
		// 不让它带着「有限制」的承诺继续跑。
		pg.kill(cmd)
		_ = cmd.Wait()
		return Result{}, wrap(plan, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var werr error
	killed := false
	select {
	case werr = <-done:
	case <-runCtx.Done():
		killed = true
		pg.kill(cmd)
		werr = <-done
	}
	if !killed {
		// 命令正常退出也要收拢进程组：`sh -c 'cmd &'` 会留下后台子进程，
		// 它们不该跟着服务端一直活着。进程组已空时这次 kill 返回 ESRCH。
		pg.kill(cmd)
	}

	res = capture.Result()
	res.Duration = time.Since(started)

	if killed {
		// 任务取消优先：调用方要能区分「用户取消」与「命令自己超时」。
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		res.TimedOut = true
	}
	if werr != nil {
		var exit *exec.ExitError
		if errors.As(werr, &exit) {
			res.ExitCode = exit.ExitCode()
		} else {
			return Result{}, wrap(plan, werr)
		}
	}
	return res, nil
}
