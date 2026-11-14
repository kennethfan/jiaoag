//go:build windows

package exec

import (
	"os/exec"
	"strconv"
)

// setProcessGroup 在 Windows 上没有 POSIX 进程组：父子关系靠 Job Object
// 或 taskkill /T 维护，这里不需要给 Cmd 设置额外属性。
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup 杀掉整棵进程树。
//
// 只杀父进程会留下它派生的子进程——go build、npm、sh 包装命令都会派生——
// 这些残留进程会继续占用端口和 CPU，服务端却以为命令已经结束。
//
// 完整的资源限制（内存/CPU/进程数）要用 Job Object，见 issue #9；这里先保证
// 「取消/超时/正常退出」都不会留下孤儿进程。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// taskkill 失败（进程已退出）是正常情况，调用方也不关心错误。
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}
