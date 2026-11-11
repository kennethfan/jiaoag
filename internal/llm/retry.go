package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// retryProvider 只做重试与退避，不解释协议。
//
// 组合顺序固定为 retry(timeout(adapter))：每次尝试独立计时，
// 重试的等待时间不计入单次尝试的超时（ADR-0008）。
type retryProvider struct {
	next     Provider
	attempts int // 总尝试次数，包含第一次

	baseDelay time.Duration
	maxDelay  time.Duration

	// sleep 与 jitter 可注入，测试用它们消除真实等待与随机性。
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func() float64 // [0, 1)
}

const (
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 8 * time.Second
)

func newRetryProvider(next Provider, retries int) *retryProvider {
	attempts := retries + 1
	if attempts < 1 {
		attempts = 1
	}
	return &retryProvider{
		next:      next,
		attempts:  attempts,
		baseDelay: defaultRetryBaseDelay,
		maxDelay:  defaultRetryMaxDelay,
		sleep:     sleepCtx,
		jitter:    rand.Float64,
	}
}

// sleepCtx 是可被 ctx 打断的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryable 只把限流与暂时性失败当作可重试。
//
// ErrContextOverflow 不在这里重试：ADR-0008 规定由上层收缩上下文后再发起一次，
// 装饰器没有资格替它改请求。
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrBadRequest) || errors.Is(err, ErrContextOverflow) {
		return false
	}
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrTransient)
}

// delay 计算第 attempt 次失败后的等待时间。
func (r *retryProvider) delay(attempt int, err error) time.Duration {
	var limited *RateLimitError
	if errors.As(err, &limited) && limited.RetryAfter > 0 {
		return limited.RetryAfter
	}
	d := r.baseDelay << (attempt - 1)
	if d > r.maxDelay || d <= 0 {
		d = r.maxDelay
	}
	// 抖动：0.5x ~ 1.5x，避免多客户端同时重试。
	return time.Duration(float64(d) * (0.5 + r.jitter()))
}

func (r *retryProvider) Chat(ctx context.Context, req Request) (Response, error) {
	var resp Response
	for attempt := 1; ; attempt++ {
		var err error
		resp, err = r.next.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if !retryable(err) || attempt >= r.attempts {
			return Response{}, err
		}
		if err := r.sleep(ctx, r.delay(attempt, err)); err != nil {
			return Response{}, err
		}
	}
}

// ChatStream 的重试有一条额外约束：一旦向调用方推送过任何 chunk，就不能重试。
// 否则调用方已经渲染出的文本会与重放的内容重复。
func (r *retryProvider) ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error {
	streamer, ok := r.next.(Streamer)
	if !ok {
		return fmt.Errorf("llm: 供应商不支持流式：%w", ErrStreamUnsupported)
	}
	for attempt := 1; ; attempt++ {
		delivered := false
		err := streamer.ChatStream(ctx, req, func(c Chunk) error {
			delivered = true
			return fn(c)
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if delivered || !retryable(err) || attempt >= r.attempts {
			return err
		}
		if err := r.sleep(ctx, r.delay(attempt, err)); err != nil {
			return err
		}
	}
}
