package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	text, truncated, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("读取不存在的记忆: %v", err)
	}
	if text != "" || truncated {
		t.Fatalf("期望空记忆，得到 %q truncated=%v", text, truncated)
	}
}

func TestLoadTrimsAndReportsTruncation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte("\n  "+strings.Repeat("记", MaxLoadBytes)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, truncated, err := Load(root)
	if err != nil {
		t.Fatalf("读取记忆: %v", err)
	}
	if !truncated {
		t.Fatal("超长记忆应标记截断")
	}
	if !strings.Contains(text, "请精简") {
		t.Fatalf("截断提示缺失：%q", text[len(text)-40:])
	}
	if strings.HasPrefix(text, "\n") {
		t.Fatal("首尾空白应被去掉")
	}
}

func TestSectionEmpty(t *testing.T) {
	if got := Section("  \n "); got != "" {
		t.Fatalf("空记忆不应产生提示层：%q", got)
	}
}

func TestSectionIncludesText(t *testing.T) {
	got := Section("构建用 make check")
	if !strings.Contains(got, "跨会话项目记忆") || !strings.Contains(got, "构建用 make check") {
		t.Fatalf("提示层内容不完整：%q", got)
	}
}

func TestFormatEntryCollapsesWhitespace(t *testing.T) {
	got := FormatEntry("  多行\n事实\t内容 ", time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
	want := "- 2026-11-23: 多行 事实 内容"
	if got != want {
		t.Fatalf("FormatEntry = %q，期望 %q", got, want)
	}
}
