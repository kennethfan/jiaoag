//go:build windows

package localshell

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// shellCommand 用 cmd /c 执行整串命令，与 Unix 侧保持同样的 shell 语义。
func shellCommand(command string) *exec.Cmd {
	return exec.Command("cmd", "/c", command)
}

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

// AssignProcessToJobObject 要求 PROCESS_SET_QUOTA 与 PROCESS_TERMINATE。
// PROCESS_SET_INFORMATION 看起来更像，但拿错了 OpenProcess 照样成功，
// 赋值时才报 Access is denied。
const (
	processSetQuota  = 0x0100
	processTerminate = 0x0001
)

// jobHandle 用 Job Object 把 cmd /c 派生的整棵树收在一个句柄下。
//
// 这是 Windows 上唯一可靠的「杀整棵树」手段：taskkill /T 依赖进程树快照，
// 父进程先退出时可能已经找不到孙进程。刻意不设 KILL_ON_JOB_CLOSE：
// 正常结束时留着后台子进程与 Unix 行为一致（!npm run dev & 不该被杀），
// 只有 Stop/超时才 TerminateJobObject。
type jobHandle struct {
	mu sync.Mutex
	h  syscall.Handle
}

// newJobHandle 建不出 Job 时返回 nil：本地命令的收尾质量会退化成 taskkill，
// 不值得让一条本来能跑的命令因此失败。
func newJobHandle() *jobHandle {
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil
	}
	return &jobHandle{h: syscall.Handle(h)}
}

// afterStart 把刚启动的 cmd.exe 放进 Job。
//
// os/exec 不暴露进程句柄，只能按 pid 重新打开；赋值发生在 Start 之后，
// 理论上存在「子进程已派生孙进程、还没赋进 Job」的窗口（微秒级），
// kill 时会用 taskkill /T 兜底。
func (j *jobHandle) afterStart(cmd *exec.Cmd) {
	if j == nil || cmd == nil || cmd.Process == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.h == 0 {
		return
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		// 快速命令（echo 之类）可能在赋值前就跑完了：对已结束的进程没有意义。
		return
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if r, _, _ := procAssignProcessToJobObject.Call(uintptr(j.h), uintptr(h)); r == 0 {
		_ = syscall.CloseHandle(j.h)
		j.h = 0
	}
}

// kill 终止 Job 里的所有进程，并用 taskkill /T 兜底覆盖赋值窗口里的派生。
func (j *jobHandle) kill(cmd *exec.Cmd) {
	if j != nil {
		j.mu.Lock()
		if j.h != 0 {
			_, _, _ = procTerminateJobObject.Call(uintptr(j.h), 1)
		}
		j.mu.Unlock()
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	// 进程已经退出时 taskkill 会失败，这是正常情况，调用方不关心。
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}

// close 释放句柄。没有 KILL_ON_JOB_CLOSE，所以关闭不会顺手杀掉后台子进程。
func (j *jobHandle) close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.h != 0 {
		_ = syscall.CloseHandle(j.h)
		j.h = 0
	}
}
