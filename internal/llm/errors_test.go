package llm

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// errReader 让 readSSE 在第一次读取时就失败。
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestClassifyTreatsTimeoutsAsTransient(t *testing.T) {
	// 408/425 是「这次没等你」，重试有意义；不能被通用 4xx 兜底成不可重试。
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly} {
		err := classifyError("openai", status, "", []byte(`{"error":{"message":"timeout"}}`))
		if !errors.Is(err, ErrTransient) {
			t.Errorf("HTTP %d 应归为 ErrTransient，实际 %v", status, err)
		}
		if errors.Is(err, ErrBadRequest) {
			t.Errorf("HTTP %d 不应归为 ErrBadRequest", status)
		}
	}
}

func TestTruncateUTF8DoesNotSplitRune(t *testing.T) {
	text := strings.Repeat("中", 200) // 600 字节
	got := truncateUTF8(text, 301)   // 301 落在第 101 个汉字中间

	if !utf8.ValidString(got) {
		t.Fatalf("截断后不是合法 UTF-8：%q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断后应以省略号结尾：%q", got)
	}
	body := strings.TrimSuffix(got, "…")
	if n := utf8.RuneCountInString(body); n != 100 {
		t.Fatalf("保留 %d 个汉字，期望 100", n)
	}
}

func TestReadSSEReadErrorIsTransient(t *testing.T) {
	err := readSSE("acme", errReader{err: errors.New("connection reset")}, func(string, []byte) error {
		return nil
	})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("读流失败应可重试，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "acme") {
		t.Fatalf("错误信息应带供应商上下文：%v", err)
	}
}

func TestReadSSECallbackErrorIsNotWrapped(t *testing.T) {
	sentinel := errors.New("调用方中止")
	err := readSSE("acme", strings.NewReader("data: x\n\n"), func(string, []byte) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("回调错误应原样透传，实际 %v", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Fatalf("回调错误不应被包装成可重试：%v", err)
	}
}

func TestReadSSETooLongLineIsNotRetryable(t *testing.T) {
	line := "data: " + strings.Repeat("a", sseMaxLineBytes+1024) + "\n\n"
	err := readSSE("acme", strings.NewReader(line), func(string, []byte) error {
		return nil
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("超长行应归为不可重试的 ErrBadRequest，实际 %v", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Fatalf("超长行不应可重试：%v", err)
	}
}
