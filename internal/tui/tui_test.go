package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompactTruncatesByRune(t *testing.T) {
	long := strings.Repeat("你好世界", 60) // 240 rune / 720 字节
	got := compact(long)

	if !utf8.ValidString(got) {
		t.Fatalf("compact 输出不是合法 UTF-8：%q", got)
	}
	if n := utf8.RuneCountInString(got); n != 161 {
		t.Fatalf("截断后 rune 数 = %d，期望 161（160 + 省略号）", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断后应以省略号结尾：%q", got)
	}
}

func TestCompactKeepsShortText(t *testing.T) {
	const raw = `{"path":"internal/app/app.go"}`
	if got := compact(raw); got != raw {
		t.Fatalf("短文本不应被改写：%q", got)
	}
}
