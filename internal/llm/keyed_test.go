package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// keyStub 是一把可编排的 key：Chat / ChatStream 各自返回预设结果。
type keyStub struct {
	name      string
	chatCalls int
	streams   int

	chatErr   error
	chunk     string
	streamErr error
	// emitBeforeErr 为真时先吐一个 chunk 再返回 streamErr，用来验证「已输出
	// 内容后不得换 key 重放」。
	emitBeforeErr bool
}

func (s *keyStub) Chat(context.Context, Request) (Response, error) {
	s.chatCalls++
	if s.chatErr != nil {
		return Response{}, s.chatErr
	}
	return Response{Message: TextMessage(RoleAssistant, s.name)}, nil
}

func (s *keyStub) ChatStream(_ context.Context, _ Request, fn func(Chunk) error) error {
	s.streams++
	if s.emitBeforeErr {
		if err := fn(Chunk{TextDelta: "partial"}); err != nil {
			return err
		}
	}
	if s.streamErr != nil {
		return s.streamErr
	}
	return fn(Chunk{TextDelta: s.chunk})
}

func newStubPool(stubs ...*keyStub) *keyedProvider {
	keys := make([]Provider, len(stubs))
	for i, s := range stubs {
		keys[i] = s
	}
	return newKeyedProvider("测试供应商", keys)
}

// 轮询：请求依次落到每把 key，再回到第一把。
func TestKeyedRoundRobin(t *testing.T) {
	a, b, c := &keyStub{name: "a"}, &keyStub{name: "b"}, &keyStub{name: "c"}
	p := newStubPool(a, b, c)
	for i := 0; i < 4; i++ {
		resp, err := p.Chat(context.Background(), Request{})
		if err != nil {
			t.Fatalf("第 %d 次调用: %v", i, err)
		}
		if got := resp.Message.Blocks[0].Text; got != string('a'+rune(i%3)) {
			t.Fatalf("第 %d 次用到 %q，期望轮询顺序 a,b,c,a", i, got)
		}
	}
	if a.chatCalls != 2 || b.chatCalls != 1 || c.chatCalls != 1 {
		t.Fatalf("调用次数 a=%d b=%d c=%d，期望 2/1/1", a.chatCalls, b.chatCalls, c.chatCalls)
	}
}

// 鉴权失败：当次换下一把，且该 key 进入冷却，后续请求不再撞它。
func TestKeyedAuthFailureEvictsKey(t *testing.T) {
	bad := &keyStub{name: "bad", chatErr: fmt.Errorf("鉴权: %w", ErrAuth)}
	good := &keyStub{name: "good"}
	p := newStubPool(bad, good)

	for i := 0; i < 3; i++ {
		if _, err := p.Chat(context.Background(), Request{}); err != nil {
			t.Fatalf("第 %d 次调用: %v", i, err)
		}
	}
	if bad.chatCalls != 1 {
		t.Fatalf("坏 key 被调用了 %d 次，期望冷却后跳过", bad.chatCalls)
	}
	if good.chatCalls != 3 {
		t.Fatalf("好 key 调用 %d 次，期望 3 次", good.chatCalls)
	}
}

// 限流：冷却时长听 Retry-After；到期后重新参与轮询。
func TestKeyedRateLimitHonorsRetryAfter(t *testing.T) {
	limited := &keyStub{name: "limited", chatErr: &RateLimitError{Provider: "p", RetryAfter: 2 * time.Minute}}
	good := &keyStub{name: "good"}
	p := newStubPool(limited, good)

	now := time.Now()
	p.now = func() time.Time { return now }
	if _, err := p.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("首次调用: %v", err)
	}
	if limited.chatCalls != 1 {
		t.Fatalf("限流 key 首次调用次数 = %d", limited.chatCalls)
	}

	now = now.Add(time.Minute)
	if _, err := p.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("冷却中调用: %v", err)
	}
	if limited.chatCalls != 1 {
		t.Fatalf("冷却期内不应再用限流 key，实际 %d 次", limited.chatCalls)
	}

	now = now.Add(2 * time.Minute)
	if _, err := p.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("冷却到期后调用: %v", err)
	}
	if limited.chatCalls != 2 {
		t.Fatalf("冷却到期后应重新尝试限流 key，实际 %d 次", limited.chatCalls)
	}
}

// 全部 key 都冷却时不能把供应商永久打死：挑最早到期的试探。
func TestKeyedAllColdStillTries(t *testing.T) {
	a := &keyStub{name: "a", chatErr: fmt.Errorf("鉴权: %w", ErrAuth)}
	b := &keyStub{name: "b", chatErr: fmt.Errorf("鉴权: %w", ErrAuth)}
	p := newStubPool(a, b)

	err := func() error { _, err := p.Chat(context.Background(), Request{}); return err }()
	if err == nil || !errors.Is(err, ErrAuth) {
		t.Fatalf("全部失败应返回可识别的错误：%v", err)
	}
	if !strings.Contains(err.Error(), "2 把 key") {
		t.Fatalf("错误信息应说明尝试了几把 key：%v", err)
	}
	before := a.chatCalls + b.chatCalls
	if _, err := p.Chat(context.Background(), Request{}); err == nil {
		t.Fatal("第二请求仍应失败")
	}
	if a.chatCalls+b.chatCalls <= before {
		t.Fatal("全部冷却后仍应挑一把 key 试探，而不是直接拒绝")
	}
}

// 流式：开流前的鉴权失败可以换 key；已输出内容后不能重放。
func TestKeyedStreamSwitchesOnlyBeforeFirstChunk(t *testing.T) {
	bad := &keyStub{name: "bad", streamErr: fmt.Errorf("鉴权: %w", ErrAuth)}
	good := &keyStub{name: "good", chunk: "ok"}
	p := newStubPool(bad, good)

	var got strings.Builder
	if err := p.ChatStream(context.Background(), Request{}, func(c Chunk) error {
		got.WriteString(c.TextDelta)
		return nil
	}); err != nil {
		t.Fatalf("开流前失败应换 key：%v", err)
	}
	if got.String() != "ok" || bad.streams != 1 || good.streams != 1 {
		t.Fatalf("输出 %q，调用 bad=%d good=%d", got.String(), bad.streams, good.streams)
	}
}

func TestKeyedStreamDoesNotReplayAfterChunk(t *testing.T) {
	broken := &keyStub{name: "broken", emitBeforeErr: true, streamErr: ErrTransient}
	backup := &keyStub{name: "backup", chunk: "ok"}
	p := newStubPool(broken, backup)

	err := p.ChatStream(context.Background(), Request{}, func(Chunk) error { return nil })
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("已输出后应原样返回错误，实际 %v", err)
	}
	if backup.streams != 0 {
		t.Fatal("已输出内容后不应换 key 重放")
	}
}

// 非可切换错误（如请求本身有问题）立即返回，不做轮换。
func TestKeyedDoesNotSwitchOnBadRequest(t *testing.T) {
	bad := &keyStub{name: "bad", chatErr: fmt.Errorf("坏请求: %w", ErrBadRequest)}
	other := &keyStub{name: "other"}
	p := newStubPool(bad, other)

	if _, err := p.Chat(context.Background(), Request{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("应返回坏请求错误：%v", err)
	}
	if other.chatCalls != 0 {
		t.Fatal("坏请求不应换 key")
	}
}
