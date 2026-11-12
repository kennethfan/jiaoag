package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/tools"
)

// fakeTool 只用于验证 Registry 的出口约束，不碰磁盘。
type fakeTool struct {
	out string
	err error
}

func (f *fakeTool) Spec() tools.Spec {
	return tools.Spec{Name: "fake", Description: "测试用工具", Version: "1", Schema: json.RawMessage(`{}`)}
}

func (f *fakeTool) Prepare(_ context.Context, call tools.Call) (tools.Prepared, error) {
	return tools.Prepared{Call: call, Spec: f.Spec()}, nil
}

func (f *fakeTool) Execute(_ context.Context, _ tools.Prepared) (tools.Result, error) {
	return tools.Result{Content: f.out}, f.err
}

func runFake(t *testing.T, out string) tools.Result {
	t.Helper()
	reg, err := tools.NewRegistry(&fakeTool{out: out})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	res, _, err := reg.Run(context.Background(), tools.Call{ID: "c1", Name: "fake", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestRunCapsHugeOutput(t *testing.T) {
	res := runFake(t, strings.Repeat("x", tools.MaxOutputBytes*2))

	if !res.Truncated {
		t.Fatal("超过硬上限应标记 Truncated")
	}
	if len(res.Content) > tools.MaxOutputBytes+200 {
		t.Fatalf("截断后仍过大：%d 字节", len(res.Content))
	}
	if !strings.Contains(res.Content, "已截断") {
		t.Fatalf("截断后应带提示，实际结尾：%q", res.Content[len(res.Content)-40:])
	}
}

func TestCapKeepsUTF8Valid(t *testing.T) {
	// 三字节字符：按字节硬切会切出半个字符。
	res := runFake(t, strings.Repeat("中", tools.MaxOutputBytes))

	if !res.Truncated {
		t.Fatal("应标记截断")
	}
	if !utf8.ValidString(res.Content) {
		t.Fatal("截断后不是合法 UTF-8")
	}
}

func TestSmallOutputIsNotTruncated(t *testing.T) {
	res := runFake(t, "短输出")
	if res.Truncated {
		t.Fatal("未超上限不应标记截断")
	}
	if res.Content != "短输出" {
		t.Fatalf("内容被改动：%q", res.Content)
	}
}
