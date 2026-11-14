package exec

import "fmt"

// Limits 是单条命令的资源约束。零值表示不限制，也就是与引入限制之前完全一致。
//
// 这些限制只作用于被执行的那条命令及其子进程，不会回头约束服务端自己：具体由
// 平台包装在 exec 之后施加（见 limits_unix.go / limits_other.go）。
type Limits struct {
	// CPUSeconds 是 CPU 时间上限，单位秒（RLIMIT_CPU）。
	CPUSeconds int `json:"cpu_seconds,omitempty"`
	// MemoryMB 是地址空间上限，单位 MB（RLIMIT_AS）。
	MemoryMB int `json:"memory_mb,omitempty"`
	// MaxFiles 是打开文件句柄上限（RLIMIT_NOFILE）。
	MaxFiles int `json:"max_files,omitempty"`
}

// MinMemoryMB 是内存上限的下限：再小连 shell 自己都起不来，没有实际意义。
const MinMemoryMB = 64

// IsZero 报告是否没有任何限制。
func (l Limits) IsZero() bool {
	return l.CPUSeconds == 0 && l.MemoryMB == 0 && l.MaxFiles == 0
}

// Validate 检查配置是否可用。零值合法。
func (l Limits) Validate() error {
	if l.CPUSeconds < 0 || l.MemoryMB < 0 || l.MaxFiles < 0 {
		return fmt.Errorf("exec: 资源限制不能为负数（cpu_seconds=%d memory_mb=%d max_files=%d）",
			l.CPUSeconds, l.MemoryMB, l.MaxFiles)
	}
	if l.MemoryMB > 0 && l.MemoryMB < MinMemoryMB {
		return fmt.Errorf("exec: memory_mb 至少为 %d，当前 %d", MinMemoryMB, l.MemoryMB)
	}
	return nil
}

// Support 描述当前平台真正能施加的限制项。
type Support struct {
	CPUSeconds bool
	MemoryMB   bool
	MaxFiles   bool
}

// Missing 返回请求了但本平台做不到的项。
//
// 用途是让审计留下「这条命令是在没有完整限制的情况下跑的」，而不是静默降级：
// 用户批准的是命令本身，不知道平台少做了什么。
func (s Support) Missing(l Limits) []string {
	var out []string
	if l.CPUSeconds > 0 && !s.CPUSeconds {
		out = append(out, "cpu_seconds")
	}
	if l.MemoryMB > 0 && !s.MemoryMB {
		out = append(out, "memory_mb")
	}
	if l.MaxFiles > 0 && !s.MaxFiles {
		out = append(out, "max_files")
	}
	return out
}
