// Package event 负责事件的两个进程内职责：向在线连接扇出、以及给应用层
// 提供一个与存储无关的发布口。
//
// 事件日志本身由 internal/store 持久化；这里不碰数据库。
package event

import (
	"sync"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// subBuffer 是每个订阅者的缓冲深度。写满意味着客户端跟不上，
// 此时关闭订阅而不是丢事件——客户端重连后按 seq 回放即可补齐。
const subBuffer = 256

// Hub 把事件扇出给当前在线的订阅者。
//
// 语义刻意做得很薄：不保证持久化、不保证送达、不做重放。重放是存储层的
// 职责（ADR-0005：先订阅进缓冲 → 回放 → 刷缓冲去重）。
type Hub struct {
	mu   sync.RWMutex
	subs map[domain.SessionID]map[*Subscription]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: make(map[domain.SessionID]map[*Subscription]struct{})}
}

// Subscription 是一个在线消费者的收件箱。Overflow 关闭后，
// 调用方应重新订阅并按 from_seq 回放。
type Subscription struct {
	ch       chan domain.Event
	overflow chan struct{}

	sessionID domain.SessionID
	hub       *Hub
	mu        sync.Mutex
	closed    bool
}

// C 返回只读事件通道。通道关闭表示订阅已失效（溢出或取消）。
func (s *Subscription) C() <-chan domain.Event { return s.ch }

// Overflow 在缓冲写满时被关闭，用于让调用方区分「正常取消」与「跟不上」。
func (s *Subscription) Overflow() <-chan struct{} { return s.overflow }

// Close 取消订阅，可重复调用。
func (s *Subscription) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.hub.remove(s)
}

// Subscribe 订阅某个会话的事件。第一个返回值最终必须 Close。
func (h *Hub) Subscribe(sessionID domain.SessionID) *Subscription {
	s := &Subscription{
		ch:        make(chan domain.Event, subBuffer),
		overflow:  make(chan struct{}),
		sessionID: sessionID,
		hub:       h,
	}
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[*Subscription]struct{})
	}
	h.subs[sessionID][s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Publish 把事件放入所有订阅者的队列。调用方必须在事务提交之后调用，
// 否则客户端可能读到尚未落库的事件。
func (h *Hub) Publish(events []domain.Event) {
	if len(events) == 0 {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ev := range events {
		for s := range h.subs[ev.SessionID] {
			s.push(ev)
		}
	}
}

func (s *Subscription) push(ev domain.Event) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	select {
	case s.ch <- ev:
		s.mu.Unlock()
	default:
		// 跟不上就断开，让客户端带 from_seq 重连；静默丢弃会造成永久缺口。
		s.closed = true
		close(s.overflow)
		close(s.ch)
		s.mu.Unlock()
		go s.hub.remove(s)
	}
}

func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	if set := h.subs[s.sessionID]; set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(h.subs, s.sessionID)
		}
	}
	h.mu.Unlock()
}

// CloseAll 在服务端关闭时断开所有订阅。
func (h *Hub) CloseAll() {
	h.mu.Lock()
	subs := make([]*Subscription, 0, len(h.subs))
	for _, set := range h.subs {
		for s := range set {
			subs = append(subs, s)
		}
	}
	h.subs = make(map[domain.SessionID]map[*Subscription]struct{})
	h.mu.Unlock()

	for _, s := range subs {
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
		s.mu.Unlock()
	}
}
