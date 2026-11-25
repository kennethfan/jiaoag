//go:build unix

package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func backgroundRegistry(t *testing.T) (*tools.Registry, *execpkg.ProcTable) {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	procs := execpkg.NewProcTable(4)
	env := tools.RuntimeEnv{
		Backend:    execpkg.NewDirect(),
		OutputDir:  t.TempDir(),
		SessionID:  "ses_bg",
		Timeout:    time.Minute,
		EventLimit: 8 << 10,
		LogLimit:   1 << 20,
		Procs:      procs,
	}
	reg, err := tools.NewRegistry(
		tools.NewRunCommand(ws, env),
		tools.NewStopCommand(env),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg, procs
}

func TestRunCommandBackgroundReturnsIDAndStops(t *testing.T) {
	reg, _ := backgroundRegistry(t)
	ctx := context.Background()
	p, err := reg.Prepare(ctx, tools.Call{
		ID:   "call_bg",
		Name: "run_command",
		Args: json.RawMessage(`{"argv":["sh","-c","echo background-ok; sleep 30"],"background":true}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	p.LogPath = filepath.Join(t.TempDir(), "bg.log")
	res, err := reg.Execute(ctx, p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "已启动后台命令") || !strings.Contains(res.Content, "call_bg") {
		t.Fatalf("启动结果应带 id：%q", res.Content)
	}
	if res.OutputPath != p.LogPath {
		t.Fatalf("后台结果应指向日志文件：%q", res.OutputPath)
	}

	stop, err := reg.Prepare(ctx, tools.Call{
		ID:   "call_stop",
		Name: "stop_command",
		Args: json.RawMessage(`{"id":"call_bg"}`),
	})
	if err != nil {
		t.Fatalf("Prepare stop: %v", err)
	}
	stopRes, err := reg.Execute(ctx, stop)
	if err != nil {
		t.Fatalf("Execute stop: %v", err)
	}
	if stopRes.IsError || !strings.Contains(stopRes.Content, "已停止") {
		t.Fatalf("停止结果不对：%+v", stopRes)
	}

	// 再停一次：进程已退出，但条目还在，仍应报告已停止（幂等）。
	stopRes, err = reg.Execute(ctx, stop)
	if err != nil {
		t.Fatalf("Execute stop 第二次: %v", err)
	}
	if stopRes.IsError {
		t.Fatalf("重复停止不应报错：%+v", stopRes)
	}
}

func TestRunCommandBackgroundRejectsTimeout(t *testing.T) {
	reg, _ := backgroundRegistry(t)
	_, err := reg.Prepare(context.Background(), tools.Call{
		ID:   "call_bad",
		Name: "run_command",
		Args: json.RawMessage(`{"argv":["sleep","1"],"background":true,"timeout_s":5}`),
	})
	if err == nil || !strings.Contains(err.Error(), "background 与 timeout_s") {
		t.Fatalf("background 与 timeout_s 同时出现应报错，实际 %v", err)
	}
}

func TestRunCommandBackgroundRespectsSessionCap(t *testing.T) {
	reg, procs := backgroundRegistry(t)
	ctx := context.Background()
	start := func(id string) {
		t.Helper()
		p, err := reg.Prepare(ctx, tools.Call{
			ID:   id,
			Name: "run_command",
			Args: json.RawMessage(`{"argv":["sh","-c","sleep 30"],"background":true}`),
		})
		if err != nil {
			t.Fatalf("Prepare %s: %v", id, err)
		}
		p.LogPath = filepath.Join(t.TempDir(), id+".log")
		if _, err := reg.Execute(ctx, p); err != nil {
			t.Fatalf("Execute %s: %v", id, err)
		}
	}
	for i := 0; i < 4; i++ {
		start([]string{"a", "b", "c", "d"}[i])
	}
	p, err := reg.Prepare(ctx, tools.Call{
		ID:   "e",
		Name: "run_command",
		Args: json.RawMessage(`{"argv":["sh","-c","sleep 30"],"background":true}`),
	})
	if err != nil {
		t.Fatalf("Prepare e: %v", err)
	}
	p.LogPath = filepath.Join(t.TempDir(), "e.log")
	res, err := reg.Execute(ctx, p)
	if err != nil {
		t.Fatalf("Execute e: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "上限") {
		t.Fatalf("第 5 个后台进程应被上限拒绝，实际 %+v", res)
	}
	_ = procs
}
