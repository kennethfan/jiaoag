package app

import (
	"context"

	"github.com/kennethfan/jiaoag/internal/store"
)

// Stats 透传存储层的只读聚合，供 /metrics 使用。
//
// 放在 App 上而不是让 server 直接拿 Store：server 只依赖 App，指标是
// 「应用层能看到的库状态」，与业务读模型同一条路径。
func (a *App) Stats(ctx context.Context) (store.Stats, error) {
	return a.store.Stats(ctx)
}
