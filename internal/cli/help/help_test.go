package help_test

import (
	"flag"
	"strings"
	"testing"

	helppkg "github.com/kennethfan/jiaoag/internal/cli/help"
)

func newFlags(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("jiaoag demo-test", flag.ContinueOnError)
	fs.String("config", "", "配置文件")
	fs.Bool("verbose", false, "多打日志")
	return fs
}

func TestWriteOrdersSections(t *testing.T) {
	var b strings.Builder
	helppkg.Write(&b, helppkg.Spec{
		Summary: "一句话说明。",
		Usage:   []string{"jiaoag demo-test [-config 文件]"},
		Commands: []helppkg.Command{
			{Name: "list", Desc: "列出"},
			{Name: "rename", Desc: "改名"},
		},
		Flags:    newFlags(t),
		Examples: []string{"jiaoag demo-test"},
	})
	out := b.String()
	for _, want := range []string{
		"一句话说明。", "用法：", "子命令：", "list", "rename", "标志：",
		"-config string", "配置文件", "-verbose", "多打日志", "示例：", "jiaoag demo-test",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("帮助里缺少 %q：\n%s", want, out)
		}
	}
	// 顺序：说明 → 用法 → 子命令 → 标志 → 示例。
	idx := func(s string) int { return strings.Index(out, s) }
	if !(idx("用法：") < idx("子命令：") && idx("子命令：") < idx("标志：") && idx("标志：") < idx("示例：")) {
		t.Fatalf("段落顺序不对：\n%s", out)
	}
}

func TestWriteSkipsEmptyFlagsSection(t *testing.T) {
	var b strings.Builder
	helppkg.Write(&b, helppkg.Spec{
		Summary: "没有标志的命令。",
		Usage:   []string{"jiaoag plugin-example"},
		Flags:   flag.NewFlagSet("jiaoag plugin-example", flag.ContinueOnError),
	})
	if strings.Contains(b.String(), "标志：") {
		t.Fatalf("没有声明标志时不该输出空标题：\n%s", b.String())
	}
}

func TestWriteWithoutOptionalSections(t *testing.T) {
	var b strings.Builder
	helppkg.Write(&b, helppkg.Spec{Summary: "只有说明。"})
	if strings.TrimSpace(b.String()) != "只有说明。" {
		t.Fatalf("多余的段落：%q", b.String())
	}
}
