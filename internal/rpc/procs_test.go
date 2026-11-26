package rpc_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/boot"
	"github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/rpc"
	"github.com/kennethfan/jiaoag/internal/store"
)

const procsConfig = `server:
  listen: 127.0.0.1:0
  token: test-token
storage:
  db_path: %DB%
  output_dir: %OUT%
policy:
  dir: %POLICY%
providers:
  - name: p
    type: openai
    base_url: http://127.0.0.1:1/v1
    model: m
    api_key: k
`

// procStack 用真实 boot 装配（含 ProcTable 与库内投影），避免为测排序再造一套桩。
func procStack(t *testing.T) (*boot.Stack, rpc.Service, string) {
	t.Helper()
	dir := t.TempDir()
	body := strings.NewReplacer(
		"%DB%", filepath.Join(dir, "data", "jiaoag.db"),
		"%OUT%", filepath.Join(dir, "out"),
		"%POLICY%", filepath.Join(dir, "policy.d"),
	).Replace(procsConfig)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置: %v", err)
	}
	stack, err := boot.Open(context.Background(), boot.Options{
		CfgPath: cfgPath,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("boot.Open: %v", err)
	}
	t.Cleanup(stack.Close)

	sess, err := stack.Core.SessionCreate(context.Background(), rpc.Owner(), protocol.SessionCreatePayload{
		Workspace: dir,
		Title:     "进程面板",
	})
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	return stack, stack.Core, sess.ID
}

func registerProc(t *testing.T, stack *boot.Stack, rec store.BackgroundProc) {
	t.Helper()
	if err := stack.DB().RegisterBackgroundProc(context.Background(), rec); err != nil {
		t.Fatalf("登记进程 %s: %v", rec.ID, err)
	}
}

func TestProcListOrdersOrphansFirst(t *testing.T) {
	stack, svc, sessionID := procStack(t)
	now := time.Now()
	registerProc(t, stack, store.BackgroundProc{
		ID: "p_run", SessionID: sessionID, Owner: stack.RuntimeID(), PID: 1,
		Argv: []string{"npm", "run", "dev"}, Status: exec.ProcRunning, StartedAt: now,
	})
	registerProc(t, stack, store.BackgroundProc{
		ID: "p_orphan", SessionID: sessionID, Owner: "rt_gone", PID: 2,
		Argv: []string{"go", "run", "."}, Status: exec.ProcOrphaned, StartedAt: now.Add(-time.Hour),
	})
	registerProc(t, stack, store.BackgroundProc{
		ID: "p_exited", SessionID: sessionID, Owner: stack.RuntimeID(), PID: 3,
		Argv: []string{"make", "watch"}, Status: exec.ProcExited, StartedAt: now.Add(time.Minute),
	})

	out, err := svc.ProcList(context.Background(), rpc.Owner(), protocol.ProcListPayload{})
	if err != nil {
		t.Fatalf("ProcList: %v", err)
	}
	got := make([]string, 0, len(out.Procs))
	for _, p := range out.Procs {
		got = append(got, p.ID+":"+p.Status)
	}
	want := []string{"p_orphan:" + exec.ProcOrphaned, "p_run:" + exec.ProcRunning, "p_exited:" + exec.ProcExited}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("排序不对\n期望 %v\n实际 %v", want, got)
	}
	if out.Procs[0].Argv[1] != "run" || out.Procs[0].Argv[2] != "." {
		t.Fatalf("命令行没带回来：%v", out.Procs[0].Argv)
	}
}

func TestProcKillRejectsRunningProcess(t *testing.T) {
	stack, svc, sessionID := procStack(t)
	registerProc(t, stack, store.BackgroundProc{
		ID: "p_live", SessionID: sessionID, Owner: stack.RuntimeID(), PID: 1,
		Status: exec.ProcRunning, StartedAt: time.Now(),
	})

	_, err := svc.ProcKill(context.Background(), rpc.Owner(), protocol.ProcActionPayload{ID: "p_live"})
	if err == nil || !strings.Contains(err.Error(), "只有属主失联") {
		t.Fatalf("运行中的进程不该被强制终止，实际：%v", err)
	}
}

func TestProcCleanDeletesStoppedRecord(t *testing.T) {
	stack, svc, sessionID := procStack(t)
	registerProc(t, stack, store.BackgroundProc{
		ID: "p_stop", SessionID: sessionID, Owner: stack.RuntimeID(), PID: 1,
		Status: exec.ProcStopped, StartedAt: time.Now(),
	})
	if _, err := svc.ProcClean(context.Background(), rpc.Owner(), protocol.ProcActionPayload{ID: "p_stop"}); err != nil {
		t.Fatalf("ProcClean: %v", err)
	}
	if _, err := stack.DB().GetBackgroundProc(context.Background(), "p_stop"); err == nil {
		t.Fatal("删除后不应该还能查到记录")
	}
}

func TestProcActionsRejectReadOnlyPrincipal(t *testing.T) {
	_, svc, _ := procStack(t)
	readonly := rpc.Principal{TokenName: "viewer", ReadOnly: true}
	if _, err := svc.ProcKill(context.Background(), readonly, protocol.ProcActionPayload{ID: "x"}); err == nil {
		t.Fatal("只读令牌不该能终止进程")
	}
	if _, err := svc.ProcClean(context.Background(), readonly, protocol.ProcActionPayload{ID: "x"}); err == nil {
		t.Fatal("只读令牌不该能删记录")
	}
}
