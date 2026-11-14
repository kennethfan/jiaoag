package llm

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// newRateProvider 在超时之内、重试之外加一层限速：每次真正发往供应商的请求
// 都要先拿到配额，因此重试同样受限速约束，不会把一次 429 放大成雪崩。
// rpm/tpm 为 0 表示该维度不限。
func newRateProvider(p Provider, rpm, tpm int) Provider {
	r := &rateProvider{provider: p, rpm: newBucket(rpm), tpm: newBucket(tpm)}
	if s, ok := p.(Streamer); ok {
		r.streamer = s
	}
	return r
}

type rateProvider struct {
	provider Provider
	streamer Streamer
	rpm      *bucket
	tpm      *bucket
}

func (r *rateProvider) Chat(ctx context.Context, req Request) (Response, error) {
	if err := r.acquire(ctx, req); err != nil {
		return Response{}, err
	}
	return r.provider.Chat(ctx, req)
}

func (r *rateProvider) ChatStream(ctx context.Context, req Request, fn func(Chunk) error) error {
	if r.streamer == nil {
		return fmt.Errorf("llm: 供应商不支持流式")
	}
	if err := r.acquire(ctx, req); err != nil {
		return err
	}
	return r.streamer.ChatStream(ctx, req, fn)
}

func (r *rateProvider) acquire(ctx context.Context, req Request) error {
	if err := r.rpm.wait(ctx, 1); err != nil {
		return err
	}
	return r.tpm.wait(ctx, float64(estimateTokens(req)))
}

// estimateTokens 是 token 数的保守估计：没有 tokenizer，就按「4 字符≈1 token」
// 加上请求的最多输出预算。宁可高估——低估会让 TPM 上限形同虚设。
func estimateTokens(req Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Result) + len(b.Args)
		}
	}
	return n/4 + req.MaxTokens
}

// bucket 是滑动补充的令牌桶：容量与补充速率都是 perMin/分钟，允许最多一分钟
// 的突发，突发用尽后按比例等待。
type bucket struct {
	mu     sync.Mutex
	perMin int
	tokens float64
	last   time.Time
}

func newBucket(perMin int) *bucket {
	if perMin <= 0 {
		return nil
	}
	return &bucket{perMin: perMin, tokens: float64(perMin), last: time.Now()}
}

// wait 申请 n 个令牌，不足时阻塞到够用；ctx 取消会立刻返回。
func (b *bucket) wait(ctx context.Context, n float64) error {
	if b == nil || n <= 0 {
		return nil
	}
	// 单次请求不能超过一分钟的额度，否则永远等不到；超出部分直接夹到上限，
	// 避免一个超大请求把整个桶饿死。
	if n > float64(b.perMin) {
		n = float64(b.perMin)
	}
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens = math.Min(float64(b.perMin), b.tokens+now.Sub(b.last).Minutes()*float64(b.perMin))
		b.last = now
		if b.tokens >= n {
			b.tokens -= n
			b.mu.Unlock()
			return nil
		}
		need := n - b.tokens
		delay := time.Duration(need / float64(b.perMin) * float64(time.Minute))
		if delay <= 0 {
			delay = time.Millisecond
		}
		b.mu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
