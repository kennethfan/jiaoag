//go:build windows

package tools

import "testing"

// 展示路径在 Windows 上必须是正斜杠：审批面板、proposal.applied 与
// write.applied 的 detail 都直接用它，混着反斜杠的 `a/notes\a.md` 既难读，
// 也让人怀疑写入目标到底是哪个目录。
func TestDisplayPathUsesForwardSlashes(t *testing.T) {
	cases := []struct{ alias, rel, want string }{
		{"", `notes\a.md`, "notes/a.md"},
		{"lib", `notes\a.md`, "lib:notes/a.md"},
		{"", "notes/a.md", "notes/a.md"},
	}
	for _, c := range cases {
		if got := displayPath(c.alias, c.rel); got != c.want {
			t.Errorf("displayPath(%q, %q) = %q，期望 %q", c.alias, c.rel, got, c.want)
		}
	}
}
