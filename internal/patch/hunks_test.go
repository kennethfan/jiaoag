package patch

import (
	"strings"
	"testing"
)

const sampleDiff = `--- a/out.txt
+++ b/out.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
@@ -10,3 +10,4 @@
 ten
 eleven
 twelve
+thirteen
`

func oldSample() []byte {
	return []byte(strings.Join([]string{
		"one", "two", "three", "four", "five",
		"six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "",
	}, "\n"))
}

func TestParseHunksCountsLines(t *testing.T) {
	hunks, err := ParseHunks(sampleDiff)
	if err != nil {
		t.Fatalf("ParseHunks: %v", err)
	}
	if len(hunks) != 2 {
		t.Fatalf("期望 2 个块，得到 %d", len(hunks))
	}
	first := hunks[0]
	if first.Index != 0 || first.OldStart != 1 || first.OldCount != 3 {
		t.Fatalf("第一块范围不对: %+v", first)
	}
	if first.Added != 1 || first.Removed != 1 {
		t.Fatalf("第一块增删行数不对: +%d -%d", first.Added, first.Removed)
	}
	second := hunks[1]
	if second.OldStart != 10 || second.NewCount != 4 || second.Added != 1 || second.Removed != 0 {
		t.Fatalf("第二块不对: %+v", second)
	}
}

func TestApplyHunksAllSelected(t *testing.T) {
	got, err := ApplyHunks(oldSample(), sampleDiff, nil)
	if err != nil {
		t.Fatalf("ApplyHunks: %v", err)
	}
	want := "one\nTWO\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\ntwelve\nthirteen\n"
	if string(got) != want {
		t.Fatalf("内容不符\n got: %q\nwant: %q", got, want)
	}
}

func TestApplyHunksSkipsUnselected(t *testing.T) {
	got, err := ApplyHunks(oldSample(), sampleDiff, []int{1})
	if err != nil {
		t.Fatalf("ApplyHunks: %v", err)
	}
	if !strings.Contains(string(got), "two\n") {
		t.Fatalf("未选中的第一块不该生效: %q", got)
	}
	if !strings.Contains(string(got), "thirteen\n") {
		t.Fatalf("选中的第二块应当生效: %q", got)
	}
	// 只应用第二块时，第一块附近的原始内容必须原样保留。
	if !strings.HasPrefix(string(got), "one\ntwo\nthree\n") {
		t.Fatalf("前缀被改坏了: %q", got)
	}
}

func TestApplyHunksKeepsMissingTrailingNewline(t *testing.T) {
	old := []byte("a\nb\nc")
	diff, truncated, err := Unified("x.txt", old, []byte("a\nB\nc"), true)
	if err != nil || truncated {
		t.Fatalf("Unified: err=%v truncated=%v", err, truncated)
	}
	got, err := ApplyHunks(old, diff, nil)
	if err != nil {
		t.Fatalf("ApplyHunks: %v", err)
	}
	if string(got) != "a\nB\nc" {
		t.Fatalf("应保留无尾换行: %q", got)
	}
}

func TestApplyHunksNewFile(t *testing.T) {
	diff, _, err := Unified("new.txt", nil, []byte("hello\nworld\n"), false)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	got, err := ApplyHunks(nil, diff, nil)
	if err != nil {
		t.Fatalf("ApplyHunks: %v", err)
	}
	if string(got) != "hello\nworld\n" {
		t.Fatalf("新文件内容不对: %q", got)
	}
}

func TestApplyHunksRejectsChangedBase(t *testing.T) {
	old := []byte("a\nXX\nc\n")
	if _, err := ApplyHunks(old, sampleDiff, nil); err == nil {
		t.Fatal("旧内容对不上时应当报错")
	}
}

func TestApplyHunksRejectsOutOfRangeSelection(t *testing.T) {
	if _, err := ApplyHunks(oldSample(), sampleDiff, []int{5}); err == nil {
		t.Fatal("越界的块下标应当报错")
	}
}

func TestApplyHunksRejectsTruncatedDiff(t *testing.T) {
	diff := sampleDiff + "[已截断：完整 diff 999 字节，仅保留前 256 KB]\n"
	if _, err := ApplyHunks(oldSample(), diff, nil); err == nil {
		t.Fatal("截断的 diff 应当拒绝应用")
	}
}

func TestSelectionNormalizes(t *testing.T) {
	got, err := Selection(3, []int{2, 0, 2})
	if err != nil {
		t.Fatalf("Selection: %v", err)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("应去重并升序: %v", got)
	}
	all, err := Selection(2, []int{0, 1})
	if err != nil {
		t.Fatalf("Selection: %v", err)
	}
	if all != nil {
		t.Fatalf("全选应归一为 nil（整份应用）: %v", all)
	}
	if none, err := Selection(2, nil); err != nil || none != nil {
		t.Fatalf("空选择应返回 nil: %v %v", none, err)
	}
}
