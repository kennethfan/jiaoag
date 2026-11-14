//go:build windows

package exec

import (
	"os/exec"
	"strconv"
)

// taskkillTree 杀掉整棵进程树。
//
// 只杀父进程会留下它派生的子进程——go build、npm、sh 包装命令都会派生——
// 这些残留进程会继续占用端口和 CPU，服务端却以为命令已经结束。
//
// Job Object 可用时这只是兜底（覆盖「Start 已返回、进程还没赋进 Job」的窗口）；
// Job Object 不可用时这是唯一手段。
func taskkillTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// taskkill 失败（进程已退出）是正常情况，调用方也不关心错误。
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}
