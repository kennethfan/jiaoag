//go:build !unix && !windows

package exec

// supportLimits 报告本平台支持的项。
//
// 既不是 Unix 也不是 Windows 的平台还没有实现资源限制：这里如实报告
// 「一项都做不到」，由调用方降级并写进审计，不能假装成功。
func supportLimits() Support { return Support{} }

// wrapLimits 在本平台原样返回 argv：没有可用的限制机制时不改动命令。
func wrapLimits(argv []string, _ Limits) []string { return argv }
