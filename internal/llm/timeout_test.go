package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// blockingProvider 一直阻塞到 ctx 结束，用来触发超时。
type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, _ Request) (Response, error) {
	<-ctx.Done()
	return Response{}, ctx.Err()
}

func (blockingProvider) ChatStream(ctx context.Context, _ Request, _ func(Chunk) error) error {
	<-ctx.Done()
	return ctx.Err()
}

// trickleProvider 每隔 interval 推送一块，推送够 count 块后：
// finish 为 true 则正常结束，否则继续阻塞（触发空闲超时）。
type trickleProvider struct {
	count    int
	interval time.Duration
	finish   bool
}

func (p trickleProvider) Chat(context.Context, Request) (Response, error) { return Response{}, nil }

func (p trickleProvider) ChatStream(ctx context.Context, _ Request, fn func(Chunk) error) error {
	for i := 0; i < p.count; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.interval):
		}
		if err := fn(Chunk{TextDelta: "x"}); err != nil {
			return err
		}
	}
	if !p.finish {
		<-ctx.Done()
		return ctx.Err()
	}
	u := Usage{}
	return fn(Chunk{Usage: &u, Done: true})
}

func TestTimeoutChatFirstByte(t *testing.T) {
	tp := &timeoutProvider{next: blockingProvider{}, firstByte: 30 * time.Millisecond, idle: 30 * time.Millisecond}

	start := time.Now()
	_, err := tp.Chat(context.Background(), Request{})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTransient) {
		t.Fatalf("错误 = %v，期望可用 errors.Is 判定为 ErrTransient", err)
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息应说明超时：%v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("耗时 = %s，超时未生效", elapsed)
	}
}

func TestTimeoutStreamFirstByte(t *testing.T) {
	tp := &timeoutProvider{next: blockingProvider{}, firstByte: 30 * time.Millisecond, idle: 30 * time.Millisecond}

	start := time.Now()
	err := tp.ChatStream(context.Background(), Request{}, func(Chunk) error { return nil })
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("错误 = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("首块超时未生效，耗时 %s", time.Since(start))
	}
}

func TestTimeoutStreamIdleAfterFirstChunk(t *testing.T) {
	// 每 10ms 推一块，推 2 块后阻塞；空闲超时 100ms 应触发。
	// 余量留到 10 倍，避免重载 CI 上被调度抖动打成偶发失败。
	tp := &timeoutProvider{
		next:      trickleProvider{count: 2, interval: 10 * time.Millisecond, finish: false},
		firstByte: 500 * time.Millisecond,
		idle:      100 * time.Millisecond,
	}
	var got int
	err := tp.ChatStream(context.Background(), Request{}, func(c Chunk) error {
		if c.TextDelta != "" {
			got++
		}
		return nil
	})
	if !errors.Is(err, ErrTransient) || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误 = %v，期望空闲超时", err)
	}
	if got != 2 {
		t.Fatalf("收到 %d 块，期望 2", got)
	}
}

func TestTimeoutStreamActivityResetsIdle(t *testing.T) {
	// 共 5 块、间隔 10ms，空闲阈值 200ms：持续有数据就不该超时。
	tp := &timeoutProvider{
		next:      trickleProvider{count: 5, interval: 10 * time.Millisecond, finish: true},
		firstByte: 500 * time.Millisecond,
		idle:      200 * time.Millisecond,
	}
	var got int
	err := tp.ChatStream(context.Background(), Request{}, func(c Chunk) error {
		if c.TextDelta != "" {
			got++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("持续有增量时不应超时：%v", err)
	}
	if got != 5 {
		t.Fatalf("收到 %d 块，期望 5", got)
	}
}

func TestTimeoutParentCancelIsNotReportedAsTimeout(t *testing.T) {
	tp := &timeoutProvider{next: blockingProvider{}, firstByte: time.Minute, idle: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tp.Chat(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled（调用方取消不是超时）", err)
	}
	if strings.Contains(err.Error(), "超时") {
		t.Fatalf("调用方取消不应伪装成超时：%v", err)
	}
}

func TestTimeoutStreamPropagatesCallbackError(t *testing.T) {
	sentinel := errors.New("调用方中止")
	tp := &timeoutProvider{
		next:      trickleProvider{count: 3, interval: 5 * time.Millisecond, finish: true},
		firstByte: time.Second,
		idle:      time.Second,
	}
	err := tp.ChatStream(context.Background(), Request{}, func(Chunk) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误 = %v，期望原样返回回调错误", err)
	}
}

func TestTimeoutDefaultsClampIdle(t *testing.T) {
	tp := newTimeoutProvider(blockingProvider{}, 0)
	if tp.firstByte != 120*time.Second {
		t.Fatalf("默认首块超时 = %s，期望 120s", tp.firstByte)
	}
	if tp.idle != 60*time.Second {
		t.Fatalf("空闲超时 = %s，期望 60s（首块的一半）", tp.idle)
	}

	tp = newTimeoutProvider(blockingProvider{}, 4*time.Second)
	if tp.idle != 5*time.Second {
		t.Fatalf("空闲超时应取下限 5s，实际 %s", tp.idle)
	}
}

func TestTimeoutSlowConsumerIsNotReportedAsTimeout(t *testing.T) {
	// 调用方处理一块花的时间超过空闲阈值：这是消费慢，不是模型卡住。
	tp := &timeoutProvider{
		next:      trickleProvider{count: 2, interval: 5 * time.Millisecond, finish: true},
		firstByte: time.Second,
		idle:      50 * time.Millisecond,
	}
	var got int
	err := tp.ChatStream(context.Background(), Request{}, func(c Chunk) error {
		if c.TextDelta != "" {
			got++
		}
		time.Sleep(120 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("慢消费者不应被判为模型超时：%v", err)
	}
	if got != 2 {
		t.Fatalf("收到 %d 块，期望 2", got)
	}
}
