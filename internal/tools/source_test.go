package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/tools"
)

// stubTool 是最小的 tools.Tool：只用来观察 Spec 与聚合行为。
type stubTool struct {
	name   string
	source string
}

func (t stubTool) Spec() tools.Spec {
	return tools.Spec{Name: t.name, Version: "1", Source: t.source}
}

func (t stubTool) Prepare(context.Context, tools.Call) (tools.Prepared, error) {
	return tools.Prepared{}, nil
}

func (t stubTool) Execute(context.Context, tools.Prepared) (tools.Result, error) {
	return tools.Result{}, nil
}

// brokenSource 模拟起不来的来源（server 崩溃、插件缺失）。
type brokenSource struct {
	name string
	err  error
}

func (s brokenSource) Name() string { return s.name }

func (s brokenSource) Tools(context.Context) ([]tools.Tool, error) { return nil, s.err }

func (s brokenSource) Close() error { return nil }

func TestNewRegistryFromFillsSourceName(t *testing.T) {
	reg, errs := tools.NewRegistryFrom(context.Background(),
		tools.NewStaticSource("builtin", stubTool{name: "read_file"}, stubTool{name: "grep"}),
		tools.NewStaticSource("plugin:bar", stubTool{name: "lint"}),
	)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := map[string]string{"grep": "builtin", "lint": "plugin:bar", "read_file": "builtin"}
	for _, spec := range reg.Specs() {
		if spec.Source != want[spec.Name] {
			t.Errorf("%s 的 Source = %q，期望 %q", spec.Name, spec.Source, want[spec.Name])
		}
	}
	if names := reg.Names(); strings.Join(names, ",") != "grep,lint,read_file" {
		t.Errorf("Names() = %v，期望按名字排序", names)
	}
}

func TestNewRegistryFromKeepsToolOwnSource(t *testing.T) {
	// MCP 工具要精确到 mcp:<server>，不能被聚合时的粗粒度来源覆盖。
	reg, errs := tools.NewRegistryFrom(context.Background(),
		tools.NewStaticSource("mcp", stubTool{name: "mcp__fs__read_text_file", source: "mcp:fs"}),
	)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	spec, ok := reg.Get("mcp__fs__read_text_file")
	if !ok {
		t.Fatal("工具没有注册")
	}
	if got := spec.Spec().Source; got != "mcp:fs" {
		t.Errorf("Source = %q，期望保留工具自己的 mcp:fs", got)
	}
}

func TestNewRegistryFromSkipsBrokenSource(t *testing.T) {
	reg, errs := tools.NewRegistryFrom(context.Background(),
		brokenSource{name: "plugin:bar", err: errors.New("connection refused")},
		tools.NewStaticSource("builtin", stubTool{name: "read_file"}),
	)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "plugin:bar") {
		t.Fatalf("errs = %v，期望一条指出 plugin:bar 的错误", errs)
	}
	if _, ok := reg.Get("read_file"); !ok {
		t.Error("一个来源坏掉不该影响其余来源")
	}
}

func TestNewRegistryFromSkipsDuplicatesAndUnnamed(t *testing.T) {
	reg, errs := tools.NewRegistryFrom(context.Background(),
		tools.NewStaticSource("builtin", stubTool{name: "read_file"}),
		tools.NewStaticSource("plugin:bar", stubTool{name: "read_file"}),
		tools.NewStaticSource("plugin:baz", stubTool{name: ""}),
	)
	if len(errs) != 2 {
		t.Fatalf("errs = %v，期望重复与未命名各一条", errs)
	}
	joined := errors.Join(errs...).Error()
	for _, want := range []string{"read_file", "未命名"} {
		if !strings.Contains(joined, want) {
			t.Errorf("错误信息缺 %q：%s", want, joined)
		}
	}
	if len(reg.Names()) != 1 {
		t.Errorf("注册工具数 = %d，期望只保留第一个 read_file", len(reg.Names()))
	}
	if src := reg.Specs()[0].Source; src != "builtin" {
		t.Errorf("保留的 read_file 来源 = %q，期望 builtin（先注册者胜）", src)
	}
}

func TestStaticSourceReturnsCopy(t *testing.T) {
	src := tools.NewStaticSource("builtin", stubTool{name: "read_file"})
	batch, err := src.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	batch[0] = stubTool{name: "mutated"}

	again, _ := src.Tools(context.Background())
	if again[0].Spec().Name != "read_file" {
		t.Errorf("来源内部状态被调用方改动了：%v", again[0].Spec().Name)
	}
}
