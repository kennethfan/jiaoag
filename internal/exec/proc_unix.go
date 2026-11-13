//go:build unix

package exec

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让子进程自成一个进程组，便于整组清理。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 杀整个进程组。
//
// 只杀父进程会留下孤儿：go test / go build 都会派生编译子进程，
// 父进程死了它们还在跑，下一次取消就永远清不干净。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
