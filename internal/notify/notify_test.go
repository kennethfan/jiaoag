package notify_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/notify"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustEvent(t *testing.T, kind domain.EventKind, session domain.SessionID, task domain.TaskID, payload any) domain.Event {
	t.Helper()
	ev, err := domain.NewEvent(kind, session, task, payload)
	if err != nil {
		t.Fatalf("构造事件: %v", err)
	}
	ev.At = time.UnixMilli(1795500000000)
	return ev
}

// recorder 收集 webhook 请求。
type recorder struct {
	mu       sync.Mutex
	payloads []notify.Payload
	bodies   [][]byte
	sigs     []string
}

func (r *recorder) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var p notify.Payload
		_ = json.Unmarshal(body, &p)
		r.mu.Lock()
		r.payloads = append(r.payloads, p)
		r.bodies = append(r.bodies, body)
		r.sigs = append(r.sigs, req.Header.Get("X-Jiaoag-Signature"))
		r.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.payloads)
}

func taskQueued(task domain.TaskID, prompt string) domain.TaskQueuedPayload {
	return domain.TaskQueuedPayload{Prompt: prompt}
}

func TestDeliversApprovalAndTerminalWithPrompt(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler(http.StatusOK))
	defer srv.Close()

	d := notify.New([]notify.Hook{{Name: "ops", URL: srv.URL, Secret: "s3cr3t"}}, quietLogger())
	d.Observe(mustEvent(t, domain.EventTaskQueued, "ses_1", "tsk_1", taskQueued("tsk_1", "  修复 stats 里 Sum 的边界  ")))
	d.Observe(mustEvent(t, domain.EventApprovalRequested, "ses_1", "tsk_1", domain.ApprovalRequestedPayload{
		ApprovalID: "apr_1",
		Kind:       domain.ApprovalCommand,
		Command:    []string{"go", "test", "./..."},
	}))
	// running 不该触发通知。
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{From: domain.TaskQueued, To: domain.TaskRunning}))
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{From: domain.TaskRunning, To: domain.TaskCompleted}))
	d.Close()

	if got := rec.count(); got != 2 {
		t.Fatalf("应有 2 条通知，实际 %d", got)
	}
	first, second := rec.payloads[0], rec.payloads[1]
	if first.Kind != notify.KindApproval || first.Event != "approval.requested" {
		t.Fatalf("第一条应为审批通知，实际 %+v", first)
	}
	if first.Tool != "go" {
		t.Fatalf("命令审批的 tool 应取 argv[0]，实际 %q", first.Tool)
	}
	if first.Prompt != "修复 stats 里 Sum 的边界" {
		t.Fatalf("提示词摘要不正确：%q", first.Prompt)
	}
	if second.Kind != notify.KindCompleted || second.Status != string(domain.TaskCompleted) {
		t.Fatalf("第二条应为完成通知，实际 %+v", second)
	}
	if second.Prompt != "修复 stats 里 Sum 的边界" {
		t.Fatalf("终态通知也应带提示词摘要，实际 %q", second.Prompt)
	}

	// 签名必须能被同一 secret 复算出来。
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(rec.bodies[0])
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if rec.sigs[0] != want {
		t.Fatalf("签名不匹配：%q != %q", rec.sigs[0], want)
	}
}

func TestHookFiltersByKind(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler(http.StatusOK))
	defer srv.Close()

	d := notify.New([]notify.Hook{{
		Name: "ops", URL: srv.URL,
		Events: map[notify.Kind]bool{notify.KindFailed: true},
	}}, quietLogger())
	d.Observe(mustEvent(t, domain.EventApprovalRequested, "ses_1", "tsk_1", domain.ApprovalRequestedPayload{ApprovalID: "apr_1", Kind: domain.ApprovalDiff}))
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{To: domain.TaskCompleted}))
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{To: domain.TaskFailed}))
	d.Close()

	if got := rec.count(); got != 1 {
		t.Fatalf("只关心 failed，应收到 1 条，实际 %d", got)
	}
	if rec.payloads[0].Kind != notify.KindFailed {
		t.Fatalf("应收到失败通知，实际 %+v", rec.payloads[0])
	}
}

func TestRetriesServerErrors(t *testing.T) {
	rec := &recorder{}
	attempts := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		rec.handler(http.StatusOK)(w, r)
	}))
	defer srv.Close()

	d := notify.New([]notify.Hook{{Name: "ops", URL: srv.URL, Timeout: time.Second}}, quietLogger())
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{To: domain.TaskCompleted}))
	d.Close()

	if attempts != 3 {
		t.Fatalf("500 应重试到第 3 次成功，实际尝试 %d 次", attempts)
	}
	if rec.count() != 1 {
		t.Fatalf("成功投递应为 1 条，实际 %d", rec.count())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	attempts := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "bad payload", http.StatusBadRequest)
	}))
	defer srv.Close()

	d := notify.New([]notify.Hook{{Name: "ops", URL: srv.URL, Timeout: time.Second}}, quietLogger())
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{To: domain.TaskCompleted}))
	d.Close()

	if attempts != 1 {
		t.Fatalf("4xx 不该重试，实际尝试 %d 次", attempts)
	}
}

func TestTruncatesLongPrompt(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler(http.StatusOK))
	defer srv.Close()

	long := strings.Repeat("修复", 200)
	d := notify.New([]notify.Hook{{Name: "ops", URL: srv.URL}}, quietLogger())
	d.Observe(mustEvent(t, domain.EventTaskQueued, "ses_1", "tsk_1", taskQueued("tsk_1", long)))
	d.Observe(mustEvent(t, domain.EventTaskStatus, "ses_1", "tsk_1", domain.TaskStatusPayload{To: domain.TaskCompleted}))
	d.Close()

	if got := rec.count(); got != 1 {
		t.Fatalf("应收到 1 条，实际 %d", got)
	}
	runes := []rune(rec.payloads[0].Prompt)
	if len(runes) != 121 { // 120 + 省略号
		t.Fatalf("摘要应截断到 120 字加省略号，实际 %d 字", len(runes))
	}
	if !strings.HasSuffix(rec.payloads[0].Prompt, "…") {
		t.Fatalf("截断后应有省略号：%q", rec.payloads[0].Prompt)
	}
}
