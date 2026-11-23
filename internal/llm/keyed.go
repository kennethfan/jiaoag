package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 同一供应商配多把 key 时的失败冷却时长。
//
// 鉴权失败通常要人工处理（key 被吊销），冷却久一点避免每个请求都去撞；
// 额度耗尽可能充值后恢复，冷却短一些；限流优先听服务端给的 Retry-After。
const (
	keyAuthCooldown  = 30 * time.Minute
	keyQuotaCooldown = 10 * time.Minute
	keyRateCooldown  = time.Minute
	keyMaxCooldown   = time.Hour
)

// keyedProvider 在同一家供应商的多把 key 之间轮询：每个请求从游标处取下一把
// 健康的 key，无论成败都推进游标，把负载摊到多把 key 上。
//
// 遇到鉴权 / 额度 / 限流失败时，该 key 进入冷却并换下一把重试；其余错误
// （请求本身有问题、上下文超限、用户取消）直接返回，不做无意义的轮换。
// 冷却只存在进程内，重启后从第一把 key 重新开始。
type keyedProvider struct {
	name      string
	keys      []Provider
	streamers []Streamer

	mu            sync.Mutex
	next          int
	cooldownUntil []time.Time
	now           func() time.Time
}

// newKeyedProvider 要求 keys 非空；只有一把 key 时调用方应直接使用它。
func newKeyedProvider(name string, keys []Provider) *keyedProvider {
	p := &keyedProvider{
		name:          name,
		keys:          keys,
		cooldownUntil: make([]time.Time, len(keys)),
		now:           time.Now,
	}
	p.streamers = make([]Streamer, len(keys))
	for i, key := range keys {
		if s, ok := key.(Streamer); ok {
			p.streamers[i] = s
		}
	}
	return p
}

// switchable 判断这次失败是否值得换一把 key。
func switchable(err error) bool {
	return errors.Is(err, ErrAuth) ||
		errors.Is(err, ErrQuotaExhausted) ||
		errors.Is(err, ErrRateLimited)
}

func (k *keyedProvider) Chat(ctx context.Context, req Request) (Response, error) {
	tried := make(map[int]bool, len(k.keys))
	var lastErr error
	for len(tried) < len(k.keys) {
		idx := k.pick(tried)
		if idx < 0 {
			break
		}
		tried[idx] = true
		resp, err := k.keys[idx].Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !switchable(err) {
			return Response{}, err
		}
		k.evict(idx, err)
	}
	return Response{}, k.allFailed(lastErr)
}

func (k *keyedProvider) ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error {
	tried := make(map[int]bool, len(k.keys))
	var lastErr error
	for len(tried) < len(k.keys) {
		idx := k.pick(tried)
		if idx < 0 {
			break
		}
		if k.streamers[idx] == nil {
			return fmt.Errorf("llm: 供应商 %s 不支持流式", k.name)
		}
		tried[idx] = true
		emitted := false
		err := k.streamers[idx].ChatStream(ctx, req, func(c Chunk) error {
			emitted = true
			return fn(c)
		})
		if err == nil {
			return nil
		}
		// 已经向调用方吐过内容就不能换 key 重放：那会把同一段输出发两遍。
		// 鉴权与额度错误在开流前就会返回，不会走到这里。
		if emitted {
			return err
		}
		lastErr = err
		if !switchable(err) {
			return err
		}
		k.evict(idx, err)
	}
	return k.allFailed(lastErr)
}

// pick 返回本次尝试的 key 下标；tried 记录本轮已试过的下标。
//
// 优先取游标之后第一把未试过且不在冷却的 key。若剩下的都在冷却，挑最早
// 到期的那把清掉冷却再试——宁可撞一次已知失败，也不要让整个供应商不可用。
func (k *keyedProvider) pick(tried map[int]bool) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	n := len(k.keys)
	for off := 0; off < n; off++ {
		i := (k.next + off) % n
		if tried[i] || k.cooldownUntil[i].After(now) {
			continue
		}
		k.next = (i + 1) % n
		return i
	}
	best := -1
	for off := 0; off < n; off++ {
		i := (k.next + off) % n
		if tried[i] {
			continue
		}
		if best < 0 || k.cooldownUntil[i].Before(k.cooldownUntil[best]) {
			best = i
		}
	}
	if best >= 0 {
		k.cooldownUntil[best] = time.Time{}
		k.next = (best + 1) % n
	}
	return best
}

func (k *keyedProvider) evict(idx int, err error) {
	until := k.now().Add(keyCooldown(err))
	k.mu.Lock()
	k.cooldownUntil[idx] = until
	k.mu.Unlock()
}

// keyCooldown 把失败类别翻译成冷却时长。
func keyCooldown(err error) time.Duration {
	var limited *RateLimitError
	if errors.As(err, &limited) && limited.RetryAfter > 0 {
		return clampCooldown(limited.RetryAfter)
	}
	switch {
	case errors.Is(err, ErrAuth):
		return keyAuthCooldown
	case errors.Is(err, ErrQuotaExhausted):
		return keyQuotaCooldown
	default:
		return keyRateCooldown
	}
}

func clampCooldown(d time.Duration) time.Duration {
	if d <= 0 {
		return keyRateCooldown
	}
	if d > keyMaxCooldown {
		return keyMaxCooldown
	}
	return d
}

func (k *keyedProvider) allFailed(lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("llm: 供应商 %s 没有可用的 key", k.name)
	}
	return fmt.Errorf("llm: 供应商 %s 的 %d 把 key 均失败：%w", k.name, len(k.keys), lastErr)
}
