package app

import (
	"context"
	"fmt"

	"github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/store"
)

// BackgroundProcs 返回后台进程的库内投影。sessionID 为空时返回全部会话，
// 供 /ps 面板「孤儿优先」的视图使用。
func (a *App) BackgroundProcs(ctx context.Context, sessionID string) ([]exec.ProcRecord, error) {
	if a.procs == nil {
		return nil, fmt.Errorf("app: 后台进程表不可用")
	}
	return a.procs.ListStored(ctx, sessionID)
}

// BackgroundProc 按 id 取一条进程记录（/ps 面板的终止路径需要它的会话与启动时间）。
func (a *App) BackgroundProc(ctx context.Context, id string) (store.BackgroundProc, error) {
	return a.store.GetBackgroundProc(ctx, id)
}

// StopBackgroundProc 停一条进程：本地有句柄就直接杀；否则只投递停止意图，
// 由属主运行时在 1 秒内执行。返回值 local 说明这次是否就地停掉。
func (a *App) StopBackgroundProc(ctx context.Context, sessionID, id string) (bool, error) {
	if a.procs == nil {
		return false, fmt.Errorf("app: 后台进程表不可用")
	}
	found, local, err := a.procs.Stop(sessionID, id)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("app: 没有找到后台进程 %s", id)
	}
	return local, nil
}

// KillBackgroundProc 清理一条属主已失联的进程：先按启动时间校验 pid，
// 再终止进程组。校验失败时不杀，把原因交给调用方展示。
func (a *App) KillBackgroundProc(ctx context.Context, id string) error {
	if a.procs == nil {
		return fmt.Errorf("app: 后台进程表不可用")
	}
	rec, err := a.store.GetBackgroundProc(ctx, id)
	if err != nil {
		return err
	}
	if rec.Status != exec.ProcOrphaned {
		return fmt.Errorf("app: 只有属主失联（orphaned）的进程才需要强制终止；运行中的请用停止")
	}
	if err := a.procs.KillVerified(ctx, exec.ProcRecord{
		ID:        rec.ID,
		PID:       rec.PID,
		StartedAt: rec.StartedAt,
	}); err != nil {
		return err
	}
	return a.store.MarkBackgroundProc(ctx, id, exec.ProcStopped)
}

// CleanBackgroundProc 只删记录，不动进程。
func (a *App) CleanBackgroundProc(ctx context.Context, id string) error {
	if a.procs == nil {
		return fmt.Errorf("app: 后台进程表不可用")
	}
	return a.procs.DropRecord(ctx, id)
}
