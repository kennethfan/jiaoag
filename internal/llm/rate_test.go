package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 令牌桶按「每分钟额度」补充：耗尽后必须按缺口等够时间，而不是直接放行。
func TestBucketPaces(t *testing.T) {
	b := newBucket(60000) // 1000 次/秒
	ctx := context.Background()

	if err := b.wait(ctx, 60000); err != nil {
		t.Fatalf("耗尽额度: %v", err)
	}
	start := time.Now()
	if err := b.wait(ctx, 100); err != nil {
		t.Fatalf("等待额度: %v", err)
	}
	elapsed := time.Since(start)
	// 100/60000 分钟 = 100ms。放宽上下界，避免 CI 抖动作假红。
	if elapsed < 80*time.Millisecond {
		t.Fatalf("等待过短（%s），限速没有生效", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("等待过长（%s），补充速率算错了", elapsed)
	}
}

func TestBucketRespectsContext(t *testing.T) {
	b := newBucket(60) // 1 次/秒
	if err := b.wait(context.Background(), 60); err != nil {
		t.Fatalf("耗尽额度: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// 30 个 token 需要 30 秒；ctx 会先到期。
	err := b.wait(ctx, 30)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 context 超时，得到 %v", err)
	}
}

func TestBucketUnlimited(t *testing.T) {
	if newBucket(0) != nil {
		t.Fatal("额度为 0 应表示不限，返回 nil")
	}
	var b *bucket
	if err := b.wait(context.Background(), 1e9); err != nil {
		t.Fatalf("nil bucket 不应阻塞：%v", err)
	}
}

// 单次请求超过一分钟额度时只能夹到额度本身，否则永远等不到。
func TestBucketClampsOversizedRequest(t *testing.T) {
	b := newBucket(60)
	if err := b.wait(context.Background(), 60); err != nil {
		t.Fatalf("耗尽额度: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// 请求 600，但一分钟只有 60：夹到 60 后需要 60 秒，ctx 仍会先到期；
	// 这里只验证不会 panic 且受 ctx 约束。
	if err := b.wait(ctx, 600); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 context 超时，得到 %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	// 只给 system：4 个字符 → 1 token 估算，加上 MaxTokens。
	req := Request{System: "abcd", MaxTokens: 100}
	if got := estimateTokens(req); got != 101 {
		t.Fatalf("估算 = %d，期望 101", got)
	}
	// 没有内容时不应为负。
	if got := estimateTokens(Request{}); got != 0 {
		t.Fatalf("空请求估算 = %d，期望 0", got)
	}
}
