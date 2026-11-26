package app

import (
	"context"
	"errors"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// 多运行时调度的时间参数。
//
//   - lease：一条任务的占位有效期。执行中的运行时会持续续租；进程崩溃后
//     租约过期的任务由别的运行时标成 failed，而不是重新入队——重跑一个
//     可能已经产生副作用的任务，比失败更危险。
//   - renew：续租间隔。必须显著小于 lease，留出调度与 GC 抖动的余量。
//   - maint：清理与补位的节奏：接管过期租约、给内存队列补槽。
const (
	taskLeaseTTL   = 60 * time.Second
	taskLeaseRenew = 10 * time.Second
	maintInterval  = 15 * time.Second
)

// claimTask 在任务启动前抢占数据库里的这条 queued 任务。
//
// 返回 false 表示这一轮没抢到：任务已被别的运行时抢走、同会话还有活任务，
// 或全局并发已满。调用方应把任务放回队首，等维护循环重试。
func (a *App) claimTask(ctx context.Context, task domain.Task) bool {
	if a.owner == "" {
		// 没有注册成运行时（单元测试或未配置执行）时保持旧语义：不占位。
		return true
	}
	// 调用方（startLocked）已经持有 runtimeMu，这里不能再锁一次。
	max := 0
	if a.slots != nil {
		max = cap(a.slots)
	}
	ok, err := a.store.ClaimTask(ctx, task.ID, a.owner, time.Now().Add(taskLeaseTTL), max)
	if err != nil {
		a.logger().Error("抢占任务失败", "task", task.ID, "err", err)
		return false
	}
	return ok
}

// renewTaskLease 续租该运行时名下所有未结束任务。
func (a *App) renewTaskLease(ctx context.Context) {
	if a.owner == "" {
		return
	}
	if _, err := a.store.RenewTaskLeases(ctx, a.owner, time.Now().Add(taskLeaseTTL)); err != nil {
		a.logger().Warn("续租任务失败", "err", err)
	}
}

// startMaintenance 启动后台维护循环：续租、接管过期租约、给等槽的会话补位。
// 它随 baseCtx 结束而退出。
func (a *App) startMaintenance() {
	if a.owner == "" {
		return
	}
	ctx := a.baseCtx
	go func() {
		renew := time.NewTicker(taskLeaseRenew)
		defer renew.Stop()
		maint := time.NewTicker(maintInterval)
		defer maint.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-renew.C:
				a.renewTaskLease(ctx)
			case <-maint.C:
				a.reapExpiredLeases(ctx)
				a.runtimeMu.Lock()
				a.pumpLocked()
				a.runtimeMu.Unlock()
			}
		}
	}()
}

// reapExpiredLeases 把「租约过期」的任务标成 failed。别人的任务不重跑：
// 崩溃前它可能已经改了文件、发过请求，重复执行会放大副作用。
func (a *App) reapExpiredLeases(ctx context.Context) {
	tasks, err := a.store.ExpiredTasks(ctx, time.Now())
	if err != nil {
		a.logger().Warn("查询过期租约失败", "err", err)
		return
	}
	for _, task := range tasks {
		if task.Owner == a.owner {
			continue
		}
		err := a.failTask(ctx, task, "owner_lost: 运行这个任务的进程已失联，任务未完成")
		if err != nil {
			var te *domain.TransitionError
			if !errors.As(err, &te) {
				a.logger().Warn("接管过期任务失败", "task", task.ID, "err", err)
			}
			continue
		}
		a.logger().Warn("接管过期任务并标记失败", "task", task.ID, "owner", task.Owner)
	}
}

// renewLoop 在任务执行期间按固定间隔续租，直到 stop 关闭。
func (a *App) renewLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(taskLeaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-a.baseCtx.Done():
			return
		case <-ticker.C:
			a.renewTaskLease(a.baseCtx)
		}
	}
}

// runtimeAlive 判断某个运行时是否还有心跳。
func (a *App) runtimeAlive(ctx context.Context, id string) bool {
	live, err := a.store.LiveRuntimes(ctx, time.Now().Add(-30*time.Second))
	if err != nil {
		return false
	}
	for _, rt := range live {
		if rt.ID == id {
			return true
		}
	}
	return false
}
