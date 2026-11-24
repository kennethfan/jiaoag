//go:build unix

package tools_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
)

// shell 形式是模型表达管道/重定向的正当入口：工具把它包成 sh -c，
// 并在 Command.Shell 里留下原文，供审批面板与策略层识别。
func TestRunCommandShellFormBuildsShArgv(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "ok\n"}}
	reg, _ := commandRegistry(t, b, false)

	const script = "go test ./... 2>&1 | tail -20"
	res, p, err := reg.Run(context.Background(), call("run_command", `{"shell":"`+script+`"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("结果 = %+v", res)
	}
	if p.Command == nil {
		t.Fatal("没有产出 Command")
	}
	if p.Command.Shell != script {
		t.Fatalf("Shell = %q，期望原文", p.Command.Shell)
	}
	if want := []string{"sh", "-c", script}; !slices.Equal(p.Command.Argv, want) {
		t.Fatalf("Argv = %q，期望 %q", p.Command.Argv, want)
	}
	if len(b.plans) != 1 || !slices.Equal(b.plans[0].Argv, []string{"sh", "-c", script}) {
		t.Fatalf("执行后端收到的 argv 不对：%+v", b.plans)
	}
}

// argv 形式不受影响：Shell 为空，后端拿到的就是模型给的 token。
func TestRunCommandArgvFormLeavesShellEmpty(t *testing.T) {
	b := &fakeBackend{result: execpkg.Result{ExitCode: 0, Output: "ok\n"}}
	reg, _ := commandRegistry(t, b, false)

	_, p, err := reg.Run(context.Background(), call("run_command", `{"argv":["go","test","./..."]}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.Command.Shell != "" {
		t.Fatalf("argv 形式不应带 Shell：%q", p.Command.Shell)
	}
	if want := []string{"go", "test", "./..."}; !slices.Equal(p.Command.Argv, want) {
		t.Fatalf("Argv = %q", p.Command.Argv)
	}
}

func TestRunCommandRejectsBadShellArgs(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"neither", `{}`, "至少要给一个"},
		{"both", `{"argv":["ls"],"shell":"ls"}`, "只能二选一"},
		{"blank", `{"shell":"   "}`, "至少要给一个"},
		{"nul", `{"shell":"ls\u0000"}`, "NUL"},
		{"too long", `{"shell":"` + strings.Repeat("a", 8<<10+1) + `"}`, "超过上限"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{}
			reg, _ := commandRegistry(t, b, false)
			res, _, err := reg.Run(context.Background(), call("run_command", tc.args))
			if err != nil {
				t.Fatalf("参数错误应以工具结果返回：%v", err)
			}
			if !res.IsError {
				t.Fatalf("结果 = %+v，期望 IsError", res)
			}
			if !strings.Contains(res.Content, tc.want) {
				t.Fatalf("结果 = %q，期望含 %q", res.Content, tc.want)
			}
			if len(b.plans) != 0 {
				t.Fatal("参数不合法时不应启动进程")
			}
		})
	}
}

// schema 必须同时声明两种形态，否则模型看不出 shell 是合法入口。
func TestRunCommandSchemaAcceptsShell(t *testing.T) {
	reg, _ := commandRegistry(t, &fakeBackend{}, false)
	spec, ok := reg.Get("run_command")
	if !ok {
		t.Fatal("缺少 run_command")
	}
	schema := string(spec.Spec().Schema)
	for _, want := range []string{`"shell"`, `"argv"`, "anyOf"} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema 缺少 %s：%s", want, schema)
		}
	}
}
