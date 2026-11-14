//go:build windows

package exec

// supportLimits 报告 Windows 上真正能施加的限制项。
//
// CPU 时间与内存由 Job Object 的扩展限额施加（见 proc_windows.go）；
// 打开文件句柄上限没有对应机制，如实报告做不到，由调用方降级并写审计。
func supportLimits() Support {
	if !jobObjectSupported() {
		return Support{}
	}
	return Support{CPUSeconds: true, MemoryMB: true}
}

// wrapLimits 在 Windows 上原样返回 argv：限制由 Job Object 施加，不经过 shell。
func wrapLimits(argv []string, _ Limits) []string { return argv }
