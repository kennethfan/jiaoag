package app

import (
	"context"

	"github.com/kennethfan/jiaoag/internal/store"
)

// ServerTokens 列出现有访问令牌（含已吊销的），供服务端做热加载。
//
// 放到 App 上是因为服务端只依赖 App：它不该知道存储层长什么样，
// 也不该自己去开第二个数据库连接。
func (a *App) ServerTokens(ctx context.Context) ([]store.ServerToken, error) {
	return a.store.ServerTokens(ctx)
}

// ServerTokenVersion 是令牌集合的版本号，服务端每 2 秒看一次。
func (a *App) ServerTokenVersion(ctx context.Context) (int64, error) {
	return a.store.ServerTokenVersion(ctx)
}
