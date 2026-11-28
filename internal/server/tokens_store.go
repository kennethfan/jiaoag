package server

import (
	"context"
	"errors"
	"sync"
	"time"
)

// tokenWatchInterval 是服务端检查令牌版本号的间隔。
//
// 为什么不复用 SIGHUP：Windows 没有可用的 SIGHUP，热更新只能靠轮询；
// 两秒一次的单行查询代价可以忽略，换来的是三平台同一套行为（ADR-0060）。
const tokenWatchInterval = 2 * time.Second

// ReloadTokens 从数据库整体替换令牌集合。
//
// 三种情况要分开：
//   - 库里有未吊销令牌：正常替换；
//   - 库里有记录但全部已吊销：清空内存令牌，让所有连接都认证失败——
//     这是用户的明确意图，不能退回配置里的旧值；
//   - 库里一条记录都没有（还没播种）：保持配置那份不动。
func (s *Server) ReloadTokens(ctx context.Context) error {
	if s.app == nil {
		return errors.New("未注入应用层，跳过令牌加载")
	}
	rows, err := s.app.ServerTokens(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	active := make([]Token, 0, len(rows))
	for _, r := range rows {
		if r.Revoked {
			continue
		}
		active = append(active, Token{Name: r.Name, Hash: r.Hash, ReadOnly: r.ReadOnly})
	}
	if len(active) == 0 {
		s.clearTokens()
		return errors.New("数据库里的访问令牌全部已吊销，所有连接都会被拒绝")
	}
	return s.SetTokens(active)
}

// WatchTokens 轮询令牌版本号，变了就重载。调用方负责在 ctx 结束时退出。
func (s *Server) WatchTokens(ctx context.Context) {
	if s.app == nil {
		return
	}
	last, err := s.app.ServerTokenVersion(ctx)
	if err != nil {
		s.log.Warn("读取令牌版本失败，热更新不可用", "err", err)
		return
	}
	ticker := time.NewTicker(tokenWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v, err := s.app.ServerTokenVersion(ctx)
			if err != nil {
				continue
			}
			if v == last {
				continue
			}
			last = v
			if err := s.ReloadTokens(ctx); err != nil {
				s.log.Error("令牌热更新失败", "err", err)
				continue
			}
			names := s.TokenNames()
			readOnly := 0
			for _, t := range names {
				if t.ReadOnly {
					readOnly++
				}
			}
			s.log.Info("访问令牌已热更新", "count", len(names), "read_only", readOnly, "version", v)
		}
	}
}

// tokenLimiter 是每令牌的请求速率限制（次/分钟）。
//
// 用令牌桶而不是固定窗口：固定窗口在窗口边界会放过两倍突发，桶更平滑，
// 也天然支持「刚才用超了，等一会自动恢复」。
type tokenLimiter struct {
	mu      sync.Mutex
	limit   int
	buckets map[string]*tokenBucket
	now     func() time.Time // 测试注入
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newTokenLimiter(limit int) *tokenLimiter {
	return &tokenLimiter{limit: limit, buckets: make(map[string]*tokenBucket)}
}

// allow 消耗一个令牌的配额。limit <= 0 表示不限。
func (l *tokenLimiter) allow(name string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[name]
	capacity := float64(l.limit)
	if b == nil {
		b = &tokenBucket{tokens: capacity, last: now}
		l.buckets[name] = b
	}
	// 按秒回填：每分钟 limit 个，等于每秒 limit/60 个。
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * capacity / 60
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
