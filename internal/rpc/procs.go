package rpc

import (
	"context"
	"sort"

	"github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// ProcList 列出后台进程。排序固定为「孤儿 → 运行中 → 其它」，同一组里
// 新的在前：面板默认游标落在第一行，用户打开就能看到最需要处理的那条。
func (c *Core) ProcList(ctx context.Context, _ Principal, in protocol.ProcListPayload) (protocol.ProcListResultPayload, error) {
	recs, err := c.app.BackgroundProcs(ctx, in.SessionID)
	if err != nil {
		return protocol.ProcListResultPayload{}, internal(err)
	}
	out := protocol.ProcListResultPayload{Procs: make([]protocol.ProcInfo, 0, len(recs))}
	for _, r := range recs {
		out.Procs = append(out.Procs, protocol.ProcInfo{
			ID:         r.ID,
			SessionID:  r.SessionID,
			Owner:      r.Owner,
			PID:        r.PID,
			Argv:       r.Argv,
			OutputPath: r.OutputPath,
			Status:     r.Status,
			StartedAt:  r.StartedAt.UnixMilli(),
		})
	}
	sort.SliceStable(out.Procs, func(i, j int) bool {
		a, b := out.Procs[i], out.Procs[j]
		if ra, rb := procRank(a.Status), procRank(b.Status); ra != rb {
			return ra < rb
		}
		return a.StartedAt > b.StartedAt
	})
	return out, nil
}

func procRank(status string) int {
	switch status {
	case exec.ProcOrphaned:
		return 0
	case exec.ProcRunning:
		return 1
	default:
		return 2
	}
}

// ProcStop 停一条运行中的进程：本地句柄直接停，否则投递停止意图。
func (c *Core) ProcStop(ctx context.Context, p Principal, in protocol.ProcActionPayload) (protocol.ProcActionResultPayload, error) {
	if err := c.guardWrite(p, "proc.stop"); err != nil {
		return protocol.ProcActionResultPayload{}, err
	}
	return c.procStop(ctx, in.ID)
}

func (c *Core) procStop(ctx context.Context, id string) (protocol.ProcActionResultPayload, error) {
	rec, err := c.app.BackgroundProc(ctx, id)
	if err != nil {
		return protocol.ProcActionResultPayload{}, notFound(err)
	}
	local, err := c.app.StopBackgroundProc(ctx, rec.SessionID, id)
	if err != nil {
		return protocol.ProcActionResultPayload{}, internal(err)
	}
	msg := "已停止后台进程 " + id
	if !local {
		msg = "已向它的属主发出停止请求：" + id + "（1 秒内退出）"
	}
	return protocol.ProcActionResultPayload{ID: id, Local: local, Message: msg}, nil
}

// ProcKill 只处理属主已失联的进程，并且会先按启动时间校验 pid。
func (c *Core) ProcKill(ctx context.Context, p Principal, in protocol.ProcActionPayload) (protocol.ProcActionResultPayload, error) {
	if err := c.guardWrite(p, "proc.kill"); err != nil {
		return protocol.ProcActionResultPayload{}, err
	}
	if in.ID == "" {
		return protocol.ProcActionResultPayload{}, badRequest("proc.kill 缺少 id")
	}
	if err := c.app.KillBackgroundProc(ctx, in.ID); err != nil {
		return protocol.ProcActionResultPayload{}, badRequest("%s", err.Error())
	}
	return protocol.ProcActionResultPayload{ID: in.ID, Local: true, Message: "已校验启动时间并终止 " + in.ID}, nil
}

// ProcClean 只删记录，不动进程。
func (c *Core) ProcClean(ctx context.Context, p Principal, in protocol.ProcActionPayload) (protocol.ProcActionResultPayload, error) {
	if err := c.guardWrite(p, "proc.clean"); err != nil {
		return protocol.ProcActionResultPayload{}, err
	}
	if in.ID == "" {
		return protocol.ProcActionResultPayload{}, badRequest("proc.clean 缺少 id")
	}
	if err := c.app.CleanBackgroundProc(ctx, in.ID); err != nil {
		return protocol.ProcActionResultPayload{}, badRequest("%s", err.Error())
	}
	return protocol.ProcActionResultPayload{ID: in.ID, Local: true, Message: "已删除进程记录 " + in.ID}, nil
}
