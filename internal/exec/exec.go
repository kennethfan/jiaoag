// Package exec 是命令执行的唯一入口（ADR-0011）。
//
// 它只回答「在哪跑、跑什么」：权限判定在 internal/perm，输出收敛与落文件
// 在这里，审批在 internal/agent。工具与调用方都不直接碰 os/exec。
package exec

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotAvailable 表示请求的执行后端不存在或不可用。
//
// 降级必须显式（ADR-0011）：调用方拿到它时应当报错或让用户确认，
// 绝不能静默回退到 direct。
var ErrNotAvailable = errors.New("exec: 执行后端不可用")

// Command 是一次要执行的命令。Argv 逐 token 传递，永远不经 shell。
type Command struct {
	Argv    []string
	Cwd     string
	Timeout time.Duration
	Env     []string
}

// Plan 是后端给出的最终执行计划：既要真的执行，也要进审批与审计。
type Plan struct {
	Backend  string
	Version  string
	Argv     []string
	Cwd      string
	Env      []string
	Timeout  time.Duration
	Degraded bool
}

// Spec 是本次执行的输出约束。
//
// 三档预算见 issue #5：EventCap 决定进事件的字节数（与 agent 的 8 KB
// 截断阈值对齐），LogCap 决定全文日志的磁盘上限。
type Spec struct {
	LogPath   string // 全文日志路径；空表示不落文件
	EventCap  int    // 事件里最多保留多少字节（含截断提示）
	LogCap    int    // 日志文件上限
	HeadBytes int    // 内存保留的头部字节数
	TailBytes int    // 内存保留的尾部字节数
	// TailOnly 只保留尾部：check_command 失败时错误在末尾，头部没有信息量。
	TailOnly bool
}

// Result 是一次执行的结果。
type Result struct {
	ExitCode int
	Output   string // 头 + 截断提示 + 尾
	// Truncated 只表示进事件的载荷被 EventCap 截过，与日志文件无关：
	// 客户端据此提示「你看到的不是全部」。
	Truncated  bool
	OutputPath string
	Duration   time.Duration
	TimedOut   bool
	// LogTruncated 表示输出超过 LogCap，后半段没有写进全文日志。它与
	// Truncated 是两件事：事件可能完整、日志却已写满，反之亦然。
	LogTruncated bool
	// LogError 非空表示全文日志没有完整落盘；工具结果必须把这件事说出来，
	// 否则「输出已落文件」就是一句假话。
	LogError string
}

// Backend 是可插执行后端（ADR-0011 的 ExecBackend）。
//
// Prepare 只探测可用性与最终 argv，不执行；Run 才真正跑。
type Backend interface {
	Name() string
	Prepare(ctx context.Context, cmd Command) (Plan, error)
	Run(ctx context.Context, plan Plan, spec Spec) (Result, error)
}

// DefaultSpec 是执行工具使用的默认输出约束。
func DefaultSpec(logPath string) Spec {
	return SpecFor(logPath, 8<<10, 4<<20)
}

// SpecFor 按配置构造输出约束；零值回落默认。
func SpecFor(logPath string, eventCap, logCap int) Spec {
	if eventCap <= 0 {
		eventCap = 8 << 10
	}
	if logCap <= 0 {
		logCap = 4 << 20
	}
	return Spec{
		LogPath:   logPath,
		EventCap:  eventCap,
		LogCap:    logCap,
		HeadBytes: eventCap * 3 / 4,
		TailBytes: eventCap / 4,
	}
}

// normalize 补齐零值，避免调用方漏填导致「不截断」。
func (s Spec) normalize() Spec {
	if s.EventCap <= 0 {
		s.EventCap = 8 << 10
	}
	if s.LogCap <= 0 {
		s.LogCap = 4 << 20
	}
	if s.HeadBytes <= 0 {
		s.HeadBytes = s.EventCap * 3 / 4
	}
	if s.TailBytes <= 0 {
		s.TailBytes = s.EventCap / 4
	}
	return s
}

// wrap 给底层错误加上命令形状，便于工具结果与审计定位。
func wrap(plan Plan, err error) error {
	if len(plan.Argv) == 0 {
		return fmt.Errorf("exec: 空 argv: %w", err)
	}
	return fmt.Errorf("exec: %s: %w", plan.Argv[0], err)
}
