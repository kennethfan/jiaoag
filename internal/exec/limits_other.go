//go:build !unix

package exec

// supportLimits 报告本平台支持的项。
//
// !unix 分支（Windows）还没有实现资源限制：Job Object 的落地在单独批次里做。
// 这里如实报告「一项都做不到」，由调用方降级并写进审计，不能假装成功。
func supportLimits() Support { return Support{} }

// wrapLimits 在本平台原样返回 argv：没有可用的限制机制时不改动命令。
func wrapLimits(argv []string, _ Limits) []string { return argv }
