package tools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func writeFile(t *testing.T, ws *workspace.Workspace, rel, content string) {
	t.Helper()
	abs := filepath.Join(ws.Root(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func registryWithWorkspace(t *testing.T) (*tools.Registry, *workspace.Workspace) {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws))
	if err != nil {
		t.Fatal(err)
	}
	return reg, ws
}

func TestReadFileReturnsNumberedLines(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "a.go", "package a\n\nfunc A() {}\n")

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "read_file", Args: jsonArgs(`{"path":"a.go"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应是工具错误：%s", res.Content)
	}
	for _, want := range []string{"1: package a", "3: func A() {}"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, res.Content)
		}
	}
}

func TestReadFileInvalidArgsIsToolError(t *testing.T) {
	reg, _ := registryWithWorkspace(t)

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "read_file", Args: jsonArgs(`{}`),
	})
	if err != nil {
		t.Fatalf("参数错误不应中断任务：%v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "path") {
		t.Fatalf("期望 path 缺失的工具错误，得到 %+v", res)
	}
}

func TestReadFileEscapeIsToolError(t *testing.T) {
	reg, _ := registryWithWorkspace(t)
	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "read_file", Args: jsonArgs(`{"path":"../../etc/passwd"}`),
	})
	if err != nil {
		t.Fatalf("越界不应中断任务：%v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "越界") {
		t.Fatalf("期望越界的工具错误，得到 %+v", res)
	}
}

func TestGrepFindsMatchesWithLocation(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "a.go", "package a\n// TODO one\n")
	writeFile(t, ws, "b.txt", "TODO two\n")

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "grep", Args: jsonArgs(`{"pattern":"TODO"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"a.go:2:// TODO one", "b.txt:1:TODO two"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, res.Content)
		}
	}
}

func TestGrepRespectsGlobAndCaseFlag(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "a.go", "todo here\n")
	writeFile(t, ws, "b.md", "todo there\n")

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "grep",
		Args: jsonArgs(`{"pattern":"TODO","glob":"**/*.md","case_insensitive":true}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Content, "b.md:1:") || strings.Contains(res.Content, "a.go") {
		t.Fatalf("glob 过滤不生效：\n%s", res.Content)
	}
}

func TestGrepInvalidRegexIsToolError(t *testing.T) {
	reg, _ := registryWithWorkspace(t)
	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "grep", Args: jsonArgs(`{"pattern":"[unclosed"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Fatalf("非法正则应返回工具错误，得到 %+v", res)
	}
}

func TestGlobSupportsDoubleStarAndSorts(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "b.go", "package b\n")
	writeFile(t, ws, "internal/a.go", "package a\n")
	writeFile(t, ws, "internal/nested/c.go", "package c\n")

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "glob", Args: jsonArgs(`{"pattern":"**/*.go"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Split(res.Content, "\n")
	want := []string{"b.go", "internal/a.go", "internal/nested/c.go"}
	if len(got) != len(want) {
		t.Fatalf("结果数 = %d，期望 %d：\n%s", len(got), len(want), res.Content)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

func TestGlobCapsResults(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		writeFile(t, ws, name, "package x\n")
	}
	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "glob", Args: jsonArgs(`{"pattern":"*.go","max_results":2}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Content, "已截断") {
		t.Fatalf("超过上限应提示截断：\n%s", res.Content)
	}
}

func TestUnknownToolIsToolError(t *testing.T) {
	reg, _ := registryWithWorkspace(t)
	res, _, err := reg.Run(context.Background(), tools.Call{ID: "c1", Name: "write_file", Args: jsonArgs(`{}`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "read_file") {
		t.Fatalf("未知工具应列出可用工具，得到 %+v", res)
	}
}

func TestSpecsAndDigestAreStable(t *testing.T) {
	reg, _ := registryWithWorkspace(t)
	names := reg.Names()
	if len(names) != 3 || names[0] != "glob" || names[1] != "grep" || names[2] != "read_file" {
		t.Fatalf("工具名应排序：%v", names)
	}
	first := reg.Digest()
	if again := reg.Digest(); first != again {
		t.Fatal("同一次构造的 digest 应稳定")
	}
	other, _ := registryWithWorkspace(t)
	if reg.Digest() != other.Digest() {
		t.Fatal("同名同版本的工具集 digest 应相同")
	}
}

func TestMatchRules(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/y/a.go", true},
		{"internal/*.go", "internal/a.go", true},
		{"internal/*.go", "internal/x/a.go", false},
		{"internal/**", "internal/a/b/c.go", true},
		{"./a.go", "a.go", true},
		{"a.go", "b.go", false},
	}
	for _, c := range cases {
		if got := tools.Match(c.pattern, c.name); got != c.want {
			t.Fatalf("Match(%q, %q) = %v，期望 %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestDuplicateToolRegistrationFails(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	_ = reg
	if _, err := tools.NewRegistry(tools.NewReadFile(ws), tools.NewReadFile(ws)); err == nil {
		t.Fatal("重复注册应报错")
	}
}

func TestCancelledContextPropagates(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "a.go", "package a\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := reg.Run(ctx, tools.Call{ID: "c1", Name: "read_file", Args: jsonArgs(`{"path":"a.go"}`)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应向上传播，得到 %v", err)
	}
}

func jsonArgs(s string) []byte { return []byte(s) }

func TestInvalidGlobIsToolError(t *testing.T) {
	reg, _ := registryWithWorkspace(t)
	cases := []tools.Call{
		{ID: "c1", Name: "glob", Args: jsonArgs(`{"pattern":"[abc"}`)},
		{ID: "c2", Name: "grep", Args: jsonArgs(`{"pattern":"x","glob":"[abc"}`)},
	}
	for _, call := range cases {
		res, _, err := reg.Run(context.Background(), call)
		if err != nil {
			t.Fatalf("%s: 非法 glob 不应中断任务：%v", call.Name, err)
		}
		if !res.IsError || !strings.Contains(res.Content, "glob") {
			t.Fatalf("%s: 期望 glob 语法错误的工具结果，得到 %+v", call.Name, res)
		}
	}
}

func TestGrepExactLimitIsNotTruncated(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "a.txt", "hit\nhit\n")

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "grep", Args: jsonArgs(`{"pattern":"hit","max_results":2}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Content, "已截断") {
		t.Fatalf("恰好命中上限且无更多匹配时不应提示截断：\n%s", res.Content)
	}
}

func TestGrepReportsTruncationWhenMoreMatchesExist(t *testing.T) {
	reg, ws := registryWithWorkspace(t)
	writeFile(t, ws, "a.txt", "hit\nhit\nhit\n")

	res, _, err := reg.Run(context.Background(), tools.Call{
		ID: "c1", Name: "grep", Args: jsonArgs(`{"pattern":"hit","max_results":2}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Content, "已截断") {
		t.Fatalf("还有更多匹配时应提示截断：\n%s", res.Content)
	}
	if got := strings.Count(res.Content, "a.txt:"); got != 2 {
		t.Fatalf("匹配行数 = %d，期望 2：\n%s", got, res.Content)
	}
}
