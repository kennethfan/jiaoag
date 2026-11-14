//go:build windows

package exec

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

// procGroup 是 Windows 下的「一个 Job Object」。
//
// 进程树收尾与资源限制都靠 Job Object：子进程一旦赋进 Job，它之后派生的
// 进程会自动加入同一个 Job，于是「杀 Job」就等于「杀整棵树」，不需要去猜
// 谁是谁的子进程。KILL_ON_JOB_CLOSE 保证即使我们忘了杀，句柄关闭时也不会
// 留下残留进程。
type procGroup struct {
	job    syscall.Handle
	limits Limits
}

var (
	// 不引入 golang.org/x/sys/windows：这里只需要四个过程，用 LazyDLL 直接取。
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObject          = modKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = modKernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = modKernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = modKernel32.NewProc("TerminateJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9

	// JOBOBJECT_BASIC_LIMIT_INFORMATION.LimitFlags 的位。
	jobObjectLimitProcessTime    = 0x00000002
	jobObjectLimitProcessMemory  = 0x00000100
	jobObjectLimitKillOnJobClose = 0x00002000

	// AssignProcessToJobObject 要求进程句柄至少有 PROCESS_SET_QUOTA 与
	// PROCESS_TERMINATE 权限。注意 PROCESS_SET_QUOTA 是 0x0100，0x0200 是
	// PROCESS_SET_INFORMATION——拿错权限时 OpenProcess 会成功，但随后
	// AssignProcessToJobObject 报 Access is denied。
	processSetQuota  = 0x0100
	processTerminate = 0x0001

	// LARGE_INTEGER 的 CPU 时间单位是 100ns。
	cpuTimeUnit100ns = 10_000_000
)

// errorInvalidParameter 是 OpenProcess 对「进程不存在」返回的 ERROR_INVALID_PARAMETER。
const errorInvalidParameter = syscall.Errno(87)

// jobObjectBasicLimitInformation 对应 JOBOBJECT_BASIC_LIMIT_INFORMATION。
// 字段顺序与 winnt.h 一致，直接用 unsafe 传指针，不能随意增删。
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// ioCounters 对应 IO_COUNTERS。
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobObjectExtendedLimitInformation 对应 JOBOBJECT_EXTENDED_LIMIT_INFORMATION。
type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// jobObjectSupported 报告当前架构能否安全地按 unsafe 传 JOBOBJECT 结构体。
//
// Windows 的结构体里 __int64 字段要求 8 字节对齐，而 Go 在 386 上只做 4 字节
// 对齐，传过去会错位；amd64 / arm64 则与 C 一致。
func jobObjectSupported() bool {
	return runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"
}

// newProcGroup 创建 Job Object 并把资源限制写进去。
//
// 请求了限制却建不出 Job 时返回错误：静默跑在没有限制的环境里，等于加固
// 没做，还让审计以为做了。没请求限制时 Job 只是更可靠的收尾手段，建不出
// 就退回 taskkill，不让命令因此失败。
func newProcGroup(l Limits) (*procGroup, error) {
	g := &procGroup{limits: l}
	if !jobObjectSupported() {
		// 32 位 Windows 上无法安全构造下面的结构体（见 jobObjectSupported）。
		// supportLimits 已经如实报告「不支持」，调用方据此降级并写审计；
		// 这里退到 taskkill，不额外制造失败。
		return g, nil
	}
	h, _, callErr := procCreateJobObject.Call(0, 0)
	if h == 0 {
		if l.IsZero() {
			return g, nil
		}
		return nil, fmt.Errorf("exec: 创建 Job Object 失败: %w", callErr)
	}
	g.job = syscall.Handle(h)
	if err := g.configure(l); err != nil {
		_ = syscall.CloseHandle(g.job)
		g.job = 0
		if l.IsZero() {
			return g, nil
		}
		return nil, err
	}
	return g, nil
}

// configure 把 Limits 映射成 Job Object 的扩展限额。
//
// 内存用 per-process 上限（与 Unix 的 RLIMIT_AS 语义对齐）：整个 Job 的上限
// 会随并发任务数变化，配在单条命令上没法给用户一个稳定的数字。
func (g *procGroup) configure(l Limits) error {
	var info jobObjectExtendedLimitInfo
	// KILL_ON_JOB_CLOSE 始终置上：即使正常退出路径漏了 kill，句柄关闭也会收树。
	flags := uint32(jobObjectLimitKillOnJobClose)
	if l.CPUSeconds > 0 {
		flags |= jobObjectLimitProcessTime
		info.BasicLimitInformation.PerProcessUserTimeLimit = int64(l.CPUSeconds) * cpuTimeUnit100ns
	}
	if l.MemoryMB > 0 {
		flags |= jobObjectLimitProcessMemory
		info.ProcessMemoryLimit = uintptr(l.MemoryMB) << 20
	}
	info.BasicLimitInformation.LimitFlags = flags

	r, _, callErr := procSetInformationJobObject.Call(
		uintptr(g.job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		return fmt.Errorf("exec: 设置 Job Object 限制失败: %w", callErr)
	}
	return nil
}

// beforeStart 在 Windows 上无事可做：Job Object 只能在 Start 之后赋值。
func (g *procGroup) beforeStart(*exec.Cmd) {}

// afterStart 把刚启动的子进程赋进 Job Object。
//
// os/exec 不暴露进程句柄，只能按 pid 重新打开。赋值发生在 Start 之后，理论上
// 子进程可能在赋值前就派生了孙进程，那些孙进程不在 Job 里——这个窗口是微秒
// 级，kill 时还会用 taskkill /T 兜底。彻底消除它需要 launcher 或
// CREATE_SUSPENDED + ResumeThread，留作后续。
func (g *procGroup) afterStart(cmd *exec.Cmd) error {
	if g.job == 0 || cmd.Process == nil {
		return nil
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		// OpenProcess 对「进程已经退出」返回 ERROR_INVALID_PARAMETER(87)。
		// 快速命令（echo 之类）可能在我们赋值前就跑完了：限制对已经结束的
		// 进程没有意义，不该把这种情况报成失败。
		if errno, ok := err.(syscall.Errno); ok && errno == errorInvalidParameter {
			return nil
		}
		return g.degrade(fmt.Errorf("exec: 打开子进程句柄失败: %w", err))
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	if r, _, callErr := procAssignProcessToJobObject.Call(uintptr(g.job), uintptr(h)); r == 0 {
		return g.degrade(fmt.Errorf("exec: 把子进程加入 Job Object 失败: %w", callErr))
	}
	return nil
}

// degrade 决定「赋不进 Job」是致命错误还是退回 taskkill。
//
// 请求了限制时不能退化：命令已经起来了，但限制没生效，只能杀掉并报错。
// 没请求限制时 Job 只影响收尾质量，退化为 taskkill 更合适。
func (g *procGroup) degrade(err error) error {
	if g.limits.IsZero() {
		_ = syscall.CloseHandle(g.job)
		g.job = 0
		return nil
	}
	return err
}

// kill 终止 Job 里的所有进程，并用 taskkill 兜底。
//
// TerminateJobObject 覆盖已经赋进 Job 的整棵树；taskkill 覆盖「Start 之后、
// 赋值之前」派生的漏网进程，以及 Job Object 不可用的退化路径。
func (g *procGroup) kill(cmd *exec.Cmd) {
	if g.job != 0 {
		// 第二个参数是退出码，调用方只看 wait 的结果，这里用 1 表示被强杀。
		_, _, _ = procTerminateJobObject.Call(uintptr(g.job), 1)
	}
	taskkillTree(cmd)
}

// close 释放 Job 句柄。KILL_ON_JOB_CLOSE 会把仍在 Job 里的进程一起终止。
func (g *procGroup) close() {
	if g.job != 0 {
		_ = syscall.CloseHandle(g.job)
		g.job = 0
	}
}
