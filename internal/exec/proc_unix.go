//go:build unix

package exec

import (
	"os/exec"
	"syscall"
)

// procGroup 是 Unix 下的「一个进程组」。
//
// 资源限制走 wrapLimits 的 ulimit 包装，不在这里施加；这里只负责把子进程
// 放进独立进程组，并在收尾时整组杀掉。
type procGroup struct{}

// newProcGroup 在 Unix 上不会失败：没有需要提前准备的系统资源。
func newProcGroup(Limits) (*procGroup, error) { return &procGroup{}, nil }

// beforeStart 让子进程自成一个进程组，便于整组清理。
func (g *procGroup) beforeStart(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// afterStart 在 Unix 上无事可做：进程组在 fork 时就已经确定。
func (g *procGroup) afterStart(*exec.Cmd) error { return nil }

// kill 杀整个进程组。
//
// 只杀父进程会留下孤儿：go test / go build 都会派生编译子进程，
// 父进程死了它们还在跑，下一次取消就永远清不干净。
func (g *procGroup) kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// close 在 Unix 上无事可做：进程组没有句柄要释放。
func (g *procGroup) close() {}
