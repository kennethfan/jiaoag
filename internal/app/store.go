package app

import (
	"context"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// Store 是应用层对持久化的全部要求，由 internal/store 实现。
//
// 约定：
//   - 只有应用层开启事务；存储层的每个写方法自身是原子的。
//   - 写方法只能由 App 在 App.commit 内调用。不要在其他地方直接写库并自行
//     广播：提交顺序与广播顺序脱钩后，在线订阅者会丢弃较小的 seq。
//   - 写方法接收要一并提交的事件，返回补齐 Seq 与时间后的事件副本。
//   - 读方法不保证看到正在进行中的写入（WAL 快照隔离）。
type Store interface {
	CreateSession(ctx context.Context, s domain.Session, events []domain.Event) ([]domain.Event, error)
	GetSession(ctx context.Context, id domain.SessionID) (domain.Session, error)
	ListSessions(ctx context.Context) ([]domain.Session, error)

	CreateTask(ctx context.Context, t domain.Task, events []domain.Event) ([]domain.Event, error)
	GetTask(ctx context.Context, id domain.TaskID) (domain.Task, error)
	ListTasks(ctx context.Context, sessionID domain.SessionID) ([]domain.Task, error)

	// Apply 原子地应用一次状态迁移：校验迁移、更新投影、写审计、追加事件。
	Apply(ctx context.Context, tr domain.Transition) ([]domain.Event, error)

	// Replay 返回 Seq 严格大于 from 的事件，按 Seq 升序。
	// limit <= 0 时使用默认批量（200），上限 1000。
	Replay(ctx context.Context, sessionID domain.SessionID, from domain.Seq, limit int) ([]domain.Event, error)

	LastSeq(ctx context.Context, sessionID domain.SessionID) (domain.Seq, error)

	Close() error
}
