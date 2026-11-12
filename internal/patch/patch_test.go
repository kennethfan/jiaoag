package patch_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/patch"
)

func TestUnifiedMarksNewFile(t *testing.T) {
	diff, truncated, err := patch.Unified("a/b.txt", nil, []byte("hello\nworld\n"), false)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if truncated {
		t.Fatal("小文件不该被截断")
	}
	if !strings.Contains(diff, "--- /dev/null") {
		t.Fatalf("新建文件应以 /dev/null 为源：\n%s", diff)
	}
	if !strings.Contains(diff, "+++ b/a/b.txt") {
		t.Fatalf("缺少目标文件头：\n%s", diff)
	}
	if !strings.Contains(diff, "+hello") || !strings.Contains(diff, "+world") {
		t.Fatalf("缺少新增行：\n%s", diff)
	}
	// 空源不该冒出一条删除的空行。
	for _, line := range strings.Split(diff, "\n") {
		if line == "-" {
			t.Fatalf("新建文件的 diff 里出现了空删除行：\n%s", diff)
		}
	}
}

func TestUnifiedShowsOnlyChangedLines(t *testing.T) {
	old := []byte("a\nb\nc\nd\ne\nf\ng\nh\n")
	newContent := []byte("a\nb\nc\nd\nE\nf\ng\nh\n")

	diff, _, err := patch.Unified("x.txt", old, newContent, true)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if !strings.Contains(diff, "-e") || !strings.Contains(diff, "+E") {
		t.Fatalf("缺少改动行：\n%s", diff)
	}
	added, removed := patch.Stats(diff)
	if added != 1 || removed != 1 {
		t.Fatalf("统计 = +%d -%d，期望 +1 -1\n%s", added, removed, diff)
	}
}

// 空内容不能被 difflib 当成「一个空行」：新建空文件是 0 行，不是 1 行；
// 改写已存在的空文件也不该多出上下文空行，把行号整体带偏。
func TestUnifiedEmptyContentHasNoPhantomLine(t *testing.T) {
	diff, truncated, err := patch.Unified("empty.txt", nil, nil, false)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if diff != "" || truncated {
		t.Fatalf("空文件对空文件不该有 diff，实际 %q", diff)
	}
	if added, removed := patch.Stats(diff); added != 0 || removed != 0 {
		t.Fatalf("统计 = +%d -%d，期望 +0 -0", added, removed)
	}

	diff, _, err = patch.Unified("empty.txt", nil, []byte("x\n"), true)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if !strings.Contains(diff, "@@ -0,0 +1 @@") {
		t.Fatalf("已存在的空文件应从 0 行起算：\n%s", diff)
	}
	if strings.Contains(diff, "\n \n") {
		t.Fatalf("不该多出上下文空行：\n%s", diff)
	}
}

func TestUnifiedIdenticalIsEmpty(t *testing.T) {
	diff, truncated, err := patch.Unified("x.txt", []byte("same\n"), []byte("same\n"), true)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if diff != "" || truncated {
		t.Fatalf("内容相同应得到空 diff，实际 %q", diff)
	}
}

func TestUnifiedFoldsHugeDiff(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("中文内容很长的一行，用来把 diff 撑到上限之上\n")
	}
	diff, truncated, err := patch.Unified("big.txt", nil, []byte(b.String()), false)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if !truncated {
		t.Fatal("超大 diff 应标记截断")
	}
	if len(diff) > patch.MaxDiffBytes+200 {
		t.Fatalf("截断后仍过大：%d 字节", len(diff))
	}
	if !utf8.ValidString(diff) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if !strings.Contains(diff, "已截断") {
		t.Fatal("截断后应带提示")
	}
}

func TestLineEndingsRoundTrip(t *testing.T) {
	crlf := []byte("a\r\nb\r\n")
	if got := patch.LineEndings(crlf); got != patch.CRLF {
		t.Fatalf("换行风格 = %q，期望 CRLF", got)
	}
	if got := patch.LineEndings([]byte("a\nb\n")); got != patch.LF {
		t.Fatalf("换行风格 = %q，期望 LF", got)
	}
	// 归一后再还原必须逐字节回到原样。
	back := patch.Normalize(patch.ToLF(crlf), patch.CRLF)
	if string(back) != string(crlf) {
		t.Fatalf("往返后 = %q，期望 %q", back, crlf)
	}
	// LF 文件里的 CRLF 输入应被归一，避免整篇重写。
	if got := string(patch.Normalize([]byte("a\r\nb\n"), patch.LF)); got != "a\nb\n" {
		t.Fatalf("归一结果 = %q", got)
	}
}

func TestHashDistinguishesContent(t *testing.T) {
	if patch.Hash(nil) == patch.Hash([]byte("x")) {
		t.Fatal("不同内容不该得到同一摘要")
	}
	if patch.Hash([]byte("x")) != patch.Hash([]byte("x")) {
		t.Fatal("相同内容应得到同一摘要")
	}
	if got := patch.ShortHash(patch.Hash([]byte("x"))); len(got) != 12 {
		t.Fatalf("短摘要长度 = %d", len(got))
	}
}
