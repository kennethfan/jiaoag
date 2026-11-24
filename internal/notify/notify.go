// Package notify 把「需要人」的事件推给外部 webhook。
//
// 只推两类：等审批（人不在电脑前时最要紧）与任务终态（完成、失败、取消）。
// 投递是异步的：任务不会因为 webhook 慢或挂掉而卡住，队列满了宁可丢通知也
// 不阻塞执行——通知是旁路能力，不是运行前提。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// Kind 是通知类别，配置里的 events 用它筛选。
type Kind string

const (
	KindApproval  Kind = "approval"
	KindCompleted Kind = "completed"
	KindFailed    Kind = "failed"
	KindCanceled  Kind = "canceled"
)

// Kinds 返回全部可配置的类别（配置校验与文档共用）。
func Kinds() []Kind {
	return []Kind{KindApproval, KindCompleted, KindFailed, KindCanceled}
}

const (
	// maxPromptRunes 是载荷里提示词摘要的长度上限。通知要能让人认出是哪个
	// 任务，但整段提示词不该跟着 webhook 到处跑。
	maxPromptRunes = 120

	// queueSize 是待投递队列上限。满了直接丢并记日志：宁可少一条通知，
	// 也不让 webhook 反压任务。
	queueSize = 256

	// maxAttempts 与退避：webhook 侧短时抖动不该丢通知，但重试有上限。
	maxAttempts = 3

	// maxTrackedTasks 限制记住提示词的条数，避免长跑进程无限增长。
	maxTrackedTasks = 512
)

// Hook 是一个 webhook 端点。
type Hook struct {
	Name    string
	URL     string
	Secret  string
	Events  map[Kind]bool
	Timeout time.Duration
}

// Wants 报告该端点是否关心某个类别。Events 为空表示全部。
func (h Hook) Wants(kind Kind) bool {
	if len(h.Events) == 0 {
		return true
	}
	return h.Events[kind]
}

// Payload 是 POST 的 JSON 体。
type Payload struct {
	Event      string `json:"event"`
	Kind       Kind   `json:"kind"`
	At         int64  `json:"at"`
	SessionID  string `json:"session_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Tool       string `json:"tool,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	// Prompt 是任务提示词的前 maxPromptRunes 个字，用来在手机上认出任务。
	Prompt string `json:"prompt_excerpt,omitempty"`
}

type job struct {
	payload Payload
}

// Dispatcher 负责筛选、排队与投递。
//
// 零值不可用；用 New 构造。Close 会等队列里已排队的通知投递完。
type Dispatcher struct {
	hooks  []Hook
	log    *slog.Logger
	client *http.Client

	queue chan job
	wg    sync.WaitGroup

	mu      sync.Mutex
	prompts map[string]string
	closed  bool
}

// New 构造分发器。hooks 为空时返回 nil，调用方按 nil 安全处理。
func New(hooks []Hook, log *slog.Logger) *Dispatcher {
	if len(hooks) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	d := &Dispatcher{
		hooks:   hooks,
		log:     log,
		client:  &http.Client{Timeout: 30 * time.Second},
		queue:   make(chan job, queueSize),
		prompts: make(map[string]string),
	}
	d.wg.Add(1)
	go d.loop()
	return d
}

// Observe 观察一条已落库的领域事件。实现上永不阻塞：满了就丢。
func (d *Dispatcher) Observe(ev domain.Event) {
	if d == nil {
		return
	}
	switch ev.Type {
	case domain.EventTaskQueued:
		var p domain.TaskQueuedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return
		}
		d.remember(string(ev.TaskID), p.Prompt)
	case domain.EventApprovalRequested:
		var p domain.ApprovalRequestedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return
		}
		d.enqueue(Payload{
			Event:      string(ev.Type),
			Kind:       KindApproval,
			At:         ev.At.UnixMilli(),
			SessionID:  string(ev.SessionID),
			TaskID:     string(ev.TaskID),
			ApprovalID: string(p.ApprovalID),
			Tool:       approvalTool(p),
			Prompt:     d.promptFor(string(ev.TaskID)),
		})
	case domain.EventTaskStatus:
		var p domain.TaskStatusPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return
		}
		kind, ok := terminalKind(p.To)
		if !ok {
			return
		}
		d.enqueue(Payload{
			Event:     string(ev.Type),
			Kind:      kind,
			At:        ev.At.UnixMilli(),
			SessionID: string(ev.SessionID),
			TaskID:    string(ev.TaskID),
			Status:    string(p.To),
			Prompt:    d.forget(string(ev.TaskID)),
		})
	}
}

// Close 停止接收并等待已排队的通知投递完。
func (d *Dispatcher) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	close(d.queue)
	d.wg.Wait()
}

func (d *Dispatcher) loop() {
	defer d.wg.Done()
	for j := range d.queue {
		for _, hook := range d.hooks {
			if !hook.Wants(j.payload.Kind) {
				continue
			}
			d.deliver(hook, j.payload)
		}
	}
}

func (d *Dispatcher) deliver(hook Hook, p Payload) {
	body, err := json.Marshal(p)
	if err != nil {
		d.log.Warn("通知序列化失败", "hook", hook.Name, "err", err)
		return
	}
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := d.post(ctx, hook, body, p)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		if !retryable(err) || attempt == maxAttempts {
			break
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	d.log.Warn("通知投递失败", "hook", hook.Name, "event", p.Event, "err", lastErr)
}

// statusError 保留 HTTP 状态码，供重试判断。
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.code, strings.TrimSpace(e.body))
}

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		// 4xx 是配置或契约问题，重试没意义；429 除外。
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	return true // 网络错误、超时都值得重试
}

func (d *Dispatcher) post(ctx context.Context, hook Hook, body []byte, p Payload) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "jiaoag-notify/1")
	req.Header.Set("X-Jiaoag-Event", p.Event)
	if hook.Secret != "" {
		req.Header.Set("X-Jiaoag-Signature", "sha256="+sign(hook.Secret, body))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 512))
	return &statusError{code: resp.StatusCode, body: buf.String()}
}

// sign 返回 HMAC-SHA256 的十六进制摘要，接收方用同一 secret 校验。
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Dispatcher) enqueue(p Payload) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	select {
	case d.queue <- job{payload: p}:
	default:
		d.log.Warn("通知队列已满，丢弃", "event", p.Event, "task", p.TaskID)
	}
}

func (d *Dispatcher) remember(taskID, prompt string) {
	if taskID == "" {
		return
	}
	excerpt := clipPrompt(prompt)
	if excerpt == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.prompts) >= maxTrackedTasks {
		// 简单清空而不是 LRU：通知只需要「最近这些任务」的上下文，
		// 为省几百个字符串写一棵链表不值得。
		d.prompts = make(map[string]string, maxTrackedTasks)
	}
	d.prompts[taskID] = excerpt
}

func (d *Dispatcher) promptFor(taskID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.prompts[taskID]
}

func (d *Dispatcher) forget(taskID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	excerpt := d.prompts[taskID]
	delete(d.prompts, taskID)
	return excerpt
}

func terminalKind(status domain.TaskStatus) (Kind, bool) {
	switch status {
	case domain.TaskCompleted:
		return KindCompleted, true
	case domain.TaskFailed:
		return KindFailed, true
	case domain.TaskCanceled:
		return KindCanceled, true
	default:
		return "", false
	}
}

// approvalTool 给审批通知一个「批的是哪个工具」的标签：外部工具看 Tool，
// 命令看 argv[0]，写入看路径，其他退到审批类别。
func approvalTool(p domain.ApprovalRequestedPayload) string {
	if p.Tool != "" {
		return p.Tool
	}
	if len(p.Command) > 0 {
		return p.Command[0]
	}
	if p.Path != "" {
		return p.Path
	}
	return string(p.Kind)
}

func clipPrompt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxPromptRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxPromptRunes]) + "…"
}
