package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// timeoutProvider 用「首块超时 + 空闲超时」约束一次调用，而不是总时长。
//
// 流式回答可能长达数分钟：总超时会把正常的长回答砍断，而首块/空闲超时只关心
// 「是不是卡住了」。非流式调用没有增量可观察，首块超时即整次请求的上限。
//
// 组合顺序固定为 retry(timeout(adapter))：每次重试各自计时（ADR-0008）。
type timeoutProvider struct {
	next      Provider
	firstByte time.Duration // 从发起到收到首块
	idle      time.Duration // 相邻两块之间的最长间隔
}

// timeoutError 是可被 errors.Is(err, ErrTransient) 命中的超时错误，
// 这样重试装饰器无需特判就能重试。
type timeoutError struct {
	limit time.Duration
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("llm: 等待模型响应超时（%s 无数据）", e.limit)
}

func (e *timeoutError) Unwrap() error { return ErrTransient }

// Timeout 让标准库与调用方可以用 errors.Is / net.Error 语义判断。
func (e *timeoutError) Timeout() bool { return true }

func newTimeoutProvider(next Provider, limit time.Duration) *timeoutProvider {
	if limit <= 0 {
		limit = 120 * time.Second
	}
	// 空闲超时不必与首块超时等长：首块之前没有任何数据，等待更有耐心；
	// 流已开始后卡住，恢复窗口短一些更符合直觉。取一半，下限 5 秒。
	idle := limit / 2
	if idle < 5*time.Second {
		idle = 5 * time.Second
	}
	return &timeoutProvider{next: next, firstByte: limit, idle: idle}
}

func (t *timeoutProvider) Chat(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	limit := &atomic.Int64{}
	limit.Store(int64(t.firstByte))
	timer := time.AfterFunc(t.firstByte, func() {
		cancel(&timeoutError{limit: time.Duration(limit.Load())})
	})
	defer timer.Stop()

	resp, err := t.next.Chat(ctx, req)
	if err != nil {
		return Response{}, wrapTimeoutCause(ctx, err)
	}
	return resp, nil
}

func (t *timeoutProvider) ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error {
	streamer, ok := t.next.(Streamer)
	if !ok {
		return fmt.Errorf("llm: 供应商不支持流式：%w", ErrStreamUnsupported)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	limit := &atomic.Int64{}
	limit.Store(int64(t.firstByte))
	timer := time.AfterFunc(t.firstByte, func() {
		cancel(&timeoutError{limit: time.Duration(limit.Load())})
	})
	defer timer.Stop()

	err := streamer.ChatStream(ctx, req, func(c Chunk) error {
		// 处理增量期间暂停计时：慢消费者（比如 TUI 渲染）不能算作
		// 「模型没有进展」。读完这条数据后立刻重新起表，上游的等待仍然被覆盖。
		timer.Stop()
		defer func() {
			limit.Store(int64(t.idle))
			timer.Reset(t.idle)
		}()
		return fn(c)
	})
	if err != nil {
		return wrapTimeoutCause(ctx, err)
	}
	return nil
}

// wrapTimeoutCause 把「被取消」还原成具体原因：超时错误或父 ctx 的取消。
func wrapTimeoutCause(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	var te *timeoutError
	if errors.As(cause, &te) {
		return te
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}
