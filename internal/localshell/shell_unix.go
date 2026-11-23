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

// killTree 杀整个进程组：!go test 会派生编译子进程，只杀 sh 会留下孤儿，
// 端口和临时目录也会跟着漏。
func killTree(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
