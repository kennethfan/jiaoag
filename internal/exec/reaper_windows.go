//go:build windows

package exec

import (
	"context"
	"fmt"
)

// KillVerified 在 Windows 上拒绝按启动时间校验后终止：没有与 ps 等价、
// 又能可靠比对启动时间的接口，宁可不做也不误杀。
func (t *ProcTable) KillVerified(_ context.Context, rec ProcRecord) error {
	return fmt.Errorf("exec: Windows 上不支持校验后终止，请用任务管理器清理 pid %d", rec.PID)
}
