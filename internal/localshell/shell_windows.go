//go:build windows

package localshell

import "os/exec"

// shellCommand 用 cmd /c 执行整串命令，与 Unix 侧保持同样的 shell 语义。
func shellCommand(command string) *exec.Cmd {
	return exec.Command("cmd", "/c", command)
}

// killTree 在 Windows 上只杀直接子进程：cmd.exe 派生的孙进程不会跟着退出。
// 本地命令是给人临时用的，这里不引入 Job Object 那一套；要整棵树收尾请用
// agent 的 run_command（它有 Job Object）。
func killTree(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}
