package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeProvider 按调用次数返回预设结果，用来验证重试行为。
type fakeProvider struct {
	calls    int
	chatFn   func(call int) (Response, error)
	streamFn func(call int, fn func(Chunk) error) error
}

func (f *fakeProvider) Chat(_ context.Context, _ Request) (Response, error) {
	f.calls++
	if f.chatFn == nil {
		return Response{}, nil
	}
	return f.chatFn(f.calls)
}

func (f *fakeProvider) ChatStream(_ context.Context, _ Request, fn func(Chunk) error) error {
	f.calls++
	if f.streamFn == nil {
		return nil
	}
	return f.streamFn(f.calls, fn)
}

// newTestRetry 关掉真实等待与随机抖动：抖动固定 0.5 → 倍率 1.0，
// 于是第 n 次重试的等待就是 base << (n-1)，可直接断言。
func newTestRetry(next Provider, retries int) (*retryProvider, *[]time.Duration) {
	r := newRetryProvider(next, retries)
	sleeps := make([]time.Duration, 0, retries)
	r.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	r.jitter = func() float64 { return 0.5 }
	return r, &sleeps
}

func TestRetryTransientThenSuccess(t *testing.T) {
	next := &fakeProvider{chatFn: func(call int) (Response, error) {
		if call == 1 {
			return Response{}, errors.Join(ErrTransient, errors.New("连接被重置"))
		}
		return Response{Message: TextMessage(RoleAssistant, "好了")}, nil
	}}
	r, sleeps := newTestRetry(next, 2)

	resp, err := r.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if next.calls != 2 {
		t.Fatalf("调用次数 = %d，期望 2", next.calls)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 500*time.Millisecond {
		t.Fatalf("等待序列 = %v，期望 [500ms]", *sleeps)
	}
	if len(resp.Message.Blocks) != 1 || resp.Message.Blocks[0].Text != "好了" {
		t.Fatalf("响应 = %+v", resp)
	}
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	next := &fakeProvider{chatFn: func(call int) (Response, error) {
		if call == 1 {
			return Response{}, &RateLimitError{Provider: "test", RetryAfter: 3 * time.Second, Message: "429"}
		}
		return Response{}, nil
	}}
	r, sleeps := newTestRetry(next, 2)

	if _, err := r.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 3*time.Second {
		t.Fatalf("等待序列 = %v，期望 [3s]（尊重 Retry-After）", *sleeps)
	}
}

func TestRetryBackoffGrowsAndCaps(t *testing.T) {
	next := &fakeProvider{chatFn: func(int) (Response, error) {
		return Response{}, errors.Join(ErrTransient, errors.New("boom"))
	}}
	r, sleeps := newTestRetry(next, 4)

	_, err := r.Chat(context.Background(), Request{})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("错误应为 ErrTransient，实际 %v", err)
	}
	if next.calls != 5 {
		t.Fatalf("调用次数 = %d，期望 5（首次 + 4 次重试）", next.calls)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	if len(*sleeps) != len(want) {
		t.Fatalf("等待次数 = %d，期望 %d", len(*sleeps), len(want))
	}
	for i := range want {
		if (*sleeps)[i] != want[i] {
			t.Fatalf("第 %d 次等待 = %s，期望 %s", i+1, (*sleeps)[i], want[i])
		}
	}
}

func TestRetryCapsAtMaxDelay(t *testing.T) {
	r, _ := newTestRetry(&fakeProvider{}, 10)
	// 抖动固定 0.5 → 倍率 1.0，饱和后应停在上限的 2/3，为抖动留出余量。
	if want := r.maxDelay * 2 / 3; r.delay(10, errors.Join(ErrTransient)) != want {
		t.Fatalf("延迟 = %s，期望封顶 %s", r.delay(10, errors.Join(ErrTransient)), want)
	}
}

// TestRetryDelayJitterSurvivesSaturation：退避到顶后抖动仍要能拉开差距。
// 若先到上限再抖动、抖完再夹，饱和后会出现大量完全相同的等待。
func TestRetryDelayJitterSurvivesSaturation(t *testing.T) {
	r := newRetryProvider(&fakeProvider{}, 10)

	r.jitter = func() float64 { return 0 } // 倍率 0.5
	low := r.delay(10, ErrTransient)
	r.jitter = func() float64 { return 0.999 } // 倍率 1.499
	high := r.delay(10, ErrTransient)

	if low >= high {
		t.Fatalf("饱和后抖动应仍能拉开差距：低 %s，高 %s", low, high)
	}
	// 允许 1ms 误差：上限的三分之一除不尽，整数除法向下取整。
	if low < r.maxDelay/3-time.Millisecond || high > r.maxDelay {
		t.Fatalf("等待应落在 [1/3, 1]×上限内：低 %s，高 %s，上限 %s", low, high, r.maxDelay)
	}
}

func TestRetryDoesNotRetryPermanentErrors(t *testing.T) {
	cases := map[string]error{
		"鉴权":    ErrAuth,
		"请求错误":  ErrBadRequest,
		"上下文超限": ErrContextOverflow,
	}
	for name, sentinel := range cases {
		t.Run(name, func(t *testing.T) {
			next := &fakeProvider{chatFn: func(int) (Response, error) {
				return Response{}, errors.Join(sentinel, errors.New("x"))
			}}
			r, sleeps := newTestRetry(next, 3)
			if _, err := r.Chat(context.Background(), Request{}); !errors.Is(err, sentinel) {
				t.Fatalf("错误 = %v，期望包含 %v", err, sentinel)
			}
			if next.calls != 1 {
				t.Fatalf("调用次数 = %d，这类错误不应重试", next.calls)
			}
			if len(*sleeps) != 0 {
				t.Fatalf("不应等待：%v", *sleeps)
			}
		})
	}
}

func TestRetryStopsWhenContextCanceled(t *testing.T) {
	next := &fakeProvider{chatFn: func(int) (Response, error) {
		return Response{}, context.Canceled
	}}
	r, sleeps := newTestRetry(next, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.Chat(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
	if next.calls != 1 {
		t.Fatalf("调用次数 = %d，取消后不应再发起尝试", next.calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("取消后不应等待：%v", *sleeps)
	}
}

func TestRetryStreamRetriesBeforeAnyChunk(t *testing.T) {
	next := &fakeProvider{streamFn: func(call int, fn func(Chunk) error) error {
		if call == 1 {
			return errors.Join(ErrTransient, errors.New("首块前断开"))
		}
		if err := fn(Chunk{TextDelta: "ok"}); err != nil {
			return err
		}
		u := Usage{}
		return fn(Chunk{Usage: &u, Done: true})
	}}
	r, sleeps := newTestRetry(next, 2)

	chunks, err := collectStream(t, r, Request{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if next.calls != 2 {
		t.Fatalf("调用次数 = %d，期望 2（首块前可重试）", next.calls)
	}
	if len(*sleeps) != 1 {
		t.Fatalf("等待次数 = %d，期望 1", len(*sleeps))
	}
	if got := joinedText(chunks); got != "ok" {
		t.Fatalf("文本 = %q，重试后不应重复", got)
	}
}

func TestRetryStreamStopsAfterDelivery(t *testing.T) {
	// 先推送一块再失败，模拟「已经渲染了部分内容」。
	next := &fakeProvider{streamFn: func(_ int, fn func(Chunk) error) error {
		if err := fn(Chunk{TextDelta: "部分"}); err != nil {
			return err
		}
		return errors.Join(ErrTransient, errors.New("中途断开"))
	}}
	r, sleeps := newTestRetry(next, 3)

	chunks, err := collectStream(t, r, Request{})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("错误 = %v，期望 ErrTransient", err)
	}
	if next.calls != 1 {
		t.Fatalf("调用次数 = %d，已推送过增量就不该重试", next.calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("不应等待：%v", *sleeps)
	}
	if got := joinedText(chunks); got != "部分" {
		t.Fatalf("文本 = %q", got)
	}
}

func TestRetryZeroRetriesMeansSingleAttempt(t *testing.T) {
	next := &fakeProvider{chatFn: func(int) (Response, error) {
		return Response{}, ErrTransient
	}}
	r, sleeps := newTestRetry(next, 0)
	if r.attempts != 1 {
		t.Fatalf("attempts = %d，期望 1", r.attempts)
	}
	if _, err := r.Chat(context.Background(), Request{}); !errors.Is(err, ErrTransient) {
		t.Fatalf("错误 = %v", err)
	}
	if next.calls != 1 || len(*sleeps) != 0 {
		t.Fatalf("调用次数 = %d，等待 = %v", next.calls, *sleeps)
	}
}

func TestRetrySleepCanBeCanceled(t *testing.T) {
	r := newRetryProvider(&fakeProvider{}, 1)
	r.baseDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
}

func TestRetryDelayNeverExceedsMaxAfterJitter(t *testing.T) {
	r := newRetryProvider(&fakeProvider{}, 1)
	r.jitter = func() float64 { return 0.999 } // 最大抖动
	if got := r.delay(10, ErrTransient); got > r.maxDelay {
		t.Fatalf("退避 %s 超过上限 %s", got, r.maxDelay)
	}
}
