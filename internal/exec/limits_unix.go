//go:build unix

package exec

import "fmt"

// supportLimits 报告本平台支持的项：unix 的 sh 都提供 ulimit，三项可以一次设完。
func supportLimits() Support {
	return Support{CPUSeconds: true, MemoryMB: true, MaxFiles: true}
}

// wrapLimits 把资源限制塞进启动脚本：先设 ulimit，再 exec 目标。
//
// 为什么用 shell 包装而不是在父进程里 syscall.Setrlimit：限制必须在 exec 之后
// 才作用于目标进程；在服务端进程里设会连带限制服务端自己。参数逐个经 "$@" 传递，
// 不拼字符串，也不改变目标进程看到的 argv[0]。
//
// 任何一条 ulimit 失败都直接退出 125：宁可这条命令失败，也不要在没有限制的情况下执行。
func wrapLimits(argv []string, l Limits) []string {
	if l.IsZero() || len(argv) == 0 {
		return argv
	}
	script := "set -e; "
	if l.CPUSeconds > 0 {
		script += fmt.Sprintf("ulimit -t %d; ", l.CPUSeconds)
	}
	if l.MemoryMB > 0 {
		// ulimit -v 的单位是 KB。
		script += fmt.Sprintf("ulimit -v %d; ", l.MemoryMB*1024)
	}
	if l.MaxFiles > 0 {
		script += fmt.Sprintf("ulimit -n %d; ", l.MaxFiles)
	}
	script += `exec "$0" "$@"`

	out := make([]string, 0, len(argv)+3)
	out = append(out, "sh", "-c", script, argv[0])
	out = append(out, argv[1:]...)
	return out
}
