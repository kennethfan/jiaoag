package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 归一后的错误类别。调用方（以及重试装饰器）用 errors.Is 判断，
// 不比较错误字符串。
var (
	ErrRateLimited       = errors.New("llm: 触发限流")
	ErrAuth              = errors.New("llm: 鉴权失败")
	ErrBadRequest        = errors.New("llm: 请求被拒绝")
	ErrTransient         = errors.New("llm: 暂时性失败")
	ErrContextOverflow   = errors.New("llm: 上下文超限")
	ErrStreamUnsupported = errors.New("llm: 该供应商不支持流式")
)

// RateLimitError 携带服务端建议的等待时间，重试装饰器会据此等待。
type RateLimitError struct {
	Provider   string
	RetryAfter time.Duration
	Message    string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("llm: %s 触发限流（建议等待 %s）：%s", e.Provider, e.RetryAfter, e.Message)
	}
	return fmt.Sprintf("llm: %s 触发限流：%s", e.Provider, e.Message)
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// classifyError 把 HTTP 失败翻译成归一错误类别。
func classifyError(provider string, status int, retryAfter string, body []byte) error {
	message := extractErrorMessage(body)
	after := parseRetryAfter(retryAfter)

	switch {
	case status == http.StatusTooManyRequests:
		return &RateLimitError{Provider: provider, RetryAfter: after, Message: message}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("llm: %s 鉴权失败（HTTP %d）：%s: %w", provider, status, message, ErrAuth)
	case status == http.StatusRequestTimeout || status == http.StatusTooEarly:
		// 408/425 是「这次没等你」而不是「请求本身错了」，重试有意义。
		// 必须排在通用 4xx 兜底之前。
		return fmt.Errorf("llm: %s 请求超时或过早（HTTP %d）：%s: %w", provider, status, message, ErrTransient)
	case isContextOverflow(body):
		// 上下文超限通常是 400，必须排在通用 4xx 之前识别。
		return fmt.Errorf("llm: %s 上下文超限（HTTP %d）：%s: %w", provider, status, message, ErrContextOverflow)
	case status >= 400 && status < 500:
		return fmt.Errorf("llm: %s 拒绝请求（HTTP %d）：%s: %w", provider, status, message, ErrBadRequest)
	default:
		return fmt.Errorf("llm: %s 暂时性失败（HTTP %d）：%s: %w", provider, status, message, ErrTransient)
	}
}

// extractErrorMessage 尽量从两家的错误体里取出可读信息，取不到就退回截断原文。
func extractErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	text := strings.TrimSpace(string(body))
	text = truncateUTF8(text, 300)
	if text == "" {
		return "（响应体为空）"
	}
	return text
}

// isContextOverflow 识别两家各自的上下文超限信号。
func isContextOverflow(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"context_length_exceeded",
		"maximum context length",
		"context window",
		"prompt is too long",
		"input is too long",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseRetryAfter 支持秒数与 HTTP 日期两种写法；解析失败按 0（由退避策略决定）。
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}
