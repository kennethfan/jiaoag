package store

import (
	"testing"
	"time"
)

// TestTSIsLexicographicallyOrdered 锁住定长格式：时间比较必须等于字符串比较，
// 否则 ORDER BY created_at 会在同一秒内错序。
func TestTSIsLexicographicallyOrdered(t *testing.T) {
	base := time.Date(2026, 11, 11, 13, 17, 38, 0, time.UTC)
	cases := []time.Duration{
		700 * time.Millisecond,
		750 * time.Millisecond,
		750*time.Millisecond + time.Microsecond,
		999 * time.Millisecond,
	}
	for i := 1; i < len(cases); i++ {
		prev, cur := ts(base.Add(cases[i-1])), ts(base.Add(cases[i]))
		if prev >= cur {
			t.Fatalf("字符串顺序与时间顺序不一致: %q >= %q", prev, cur)
		}
	}
	if got := len(ts(base)); got != 30 {
		t.Fatalf("时间文本长度 = %d，期望 30", got)
	}
	if ts(time.Time{}) != "" {
		t.Fatal("零值应编码为空串")
	}
}
