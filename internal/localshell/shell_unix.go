//go:build !windows

package localshell

import (
	"os/exec"
	"syscall"
)

// shellCommand 用 sh -c 执行整串命令：用户写 !ls | wc -l 时期待的是 shell
// 语义，而不是把字符串按空格切开再当 argv。
func shellCommand(command string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// jobHandle 在 Unix 上没有额外对象：进程组本身就是「整棵树」的句柄。
// 保留这个类型是为了让 localshell.go 的平台无关代码只有一份。
type jobHandle struct{}

func newJobHandle() *jobHandle { return &jobHandle{} }

// afterStart 无事可做：Setpgid 在 Start 之前就已经生效。
func (j *jobHandle) afterStart(*exec.Cmd) {}

// kill 杀整个进程组：!go test 会派生编译子进程，只杀 sh 会留下孤儿，
// 端口和临时目录也会跟着漏。
func (j *jobHandle) kill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func (j *jobHandle) close() {}
