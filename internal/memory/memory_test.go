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

// 只有主根记忆时原样返回：单根工作区的提示词不能因为多根支持而变样。
func TestCombineSinglePrimaryIsUnchanged(t *testing.T) {
	got := Combine([]Entry{{Text: "- 2026-01-01: 事实"}}, Budget)
	if got != "- 2026-01-01: 事实" {
		t.Fatalf("Combine = %q", got)
	}
}

func TestCombineLabelsEachRoot(t *testing.T) {
	got := Combine([]Entry{
		{Text: "主根事实"},
		{Label: "lib", Text: "附加根事实"},
	}, Budget)
	if !strings.Contains(got, "### 主根\n主根事实") {
		t.Fatalf("缺少主根段: %q", got)
	}
	if !strings.Contains(got, "### lib\n附加根事实") {
		t.Fatalf("缺少附加根段: %q", got)
	}
	if strings.Index(got, "主根事实") > strings.Index(got, "附加根事实") {
		t.Fatal("主根应排在前面")
	}
}

func TestCombineSkipsEmptyAndNotesDropped(t *testing.T) {
	got := Combine([]Entry{
		{Text: "   "},
		{Text: strings.Repeat("a", 300)},
		{Label: "lib", Text: "放不下"},
	}, 320)
	if strings.Contains(got, "放不下") {
		t.Fatalf("超预算的根不该出现: %q", got)
	}
	if !strings.Contains(got, "还有 1 个根的记忆因总量超过 320 字节未加载") {
		t.Fatalf("缺少未加载说明: %q", got)
	}
}
