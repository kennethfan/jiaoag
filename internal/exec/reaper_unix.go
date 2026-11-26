//go:build !windows

package exec

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procStartSkew 是「库里的启动时间」与 ps 看到的启动时间允许的偏差。
// ps 只精确到秒，留 3 秒足够吸收两次取时的误差，又不至于把同一秒复用的
// pid 认错。
const procStartSkew = 3 * time.Second

// procStartTime 是测试注入点：真实实现走 `ps -o lstart=`。
var procStartTime = psStartTime

func psStartTime(pid int) (time.Time, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return time.Time{}, err
	}
	text := strings.Join(strings.Fields(string(out)), " ")
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon Jan 2 15:04:05 2006"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析 ps 的启动时间 %q", text)
}

// KillVerified 终止一条「属主已失联」的后台进程。
//
// 只有一个 pid 是不够的：pid 会被复用，直接 kill 可能杀掉无关进程。所以
// 先拿 ps 的启动时间与库里的 started_at 对照，偏差超过 procStartSkew 就
// 拒绝。终止按进程组优先（后台进程由 startBackground 建过新进程组）。
func (t *ProcTable) KillVerified(_ context.Context, rec ProcRecord) error {
	if rec.PID <= 0 {
		return fmt.Errorf("exec: 后台进程 %s 没有可用的 pid", rec.ID)
	}
	started, err := procStartTime(rec.PID)
	if err != nil {
		return fmt.Errorf("exec: 读不到 pid %d 的启动时间（进程可能已退出）", rec.PID)
	}
	if rec.StartedAt.IsZero() || absDuration(started.Sub(rec.StartedAt)) > procStartSkew {
		return fmt.Errorf("exec: pid %d 的启动时间与记录不一致，拒绝终止（pid 可能已被复用）", rec.PID)
	}
	if err := syscall.Kill(-rec.PID, syscall.SIGKILL); err != nil {
		if err := syscall.Kill(rec.PID, syscall.SIGKILL); err != nil {
			return fmt.Errorf("exec: 终止 pid %d 失败：%w", rec.PID, err)
		}
	}
	t.dropHandle(rec.ID)
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
