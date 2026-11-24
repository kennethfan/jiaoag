package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// newRootsAgent 建一个带附加根的 agent：主根有 a.txt，附加根 lib 有 b.txt。
func newRootsAgent(t *testing.T, provider *scriptedProvider, rec *fakeRecorder) (*agent.Agent, domain.Task, string, string) {
	t.Helper()
	primaryDir, libDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(primaryDir, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "b.txt"), []byte("beta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(primaryDir)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := workspace.NewRoots(ws, []workspace.NamedRoot{{Alias: "lib", Path: libDir}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(
		tools.NewReadFileWithRoots(ws, roots),
		tools.NewWriteFileWithRoots(ws, roots),
		tools.NewEditFileWithRoots(ws, roots),
	)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Options{
		Provider:      provider,
		Tools:         reg,
		Workspace:     ws,
		Roots:         roots,
		Recorder:      rec,
		Model:         "test-model",
		MaxRounds:     4,
		ContextBudget: 60000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	task := domain.Task{ID: "tsk_1", SessionID: "ses_1", Status: domain.TaskRunning, Prompt: "改附加根"}
	return ag, task, primaryDir, libDir
}

// TestApproveWritesAdditionalRoot 验证附加根的写入走完整审批链路：
// diff 带 alias 前缀、落盘落在附加根、审计能追到 root。
func TestApproveWritesAdditionalRoot(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("c1", "lib:b.txt", "beta-2\\n"),
		finalRound("改好了"),
	}}
	ag, task, primaryDir, libDir := newRootsAgent(t, provider, rec)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := mustRead(t, libDir, "b.txt"); got != "beta-2\n" {
		t.Fatalf("附加根内容不对：%q", got)
	}
	if _, err := os.Stat(filepath.Join(primaryDir, "b.txt")); err == nil {
		t.Fatal("附加根的文件不该出现在主根")
	}

	var requested domain.ApprovalRequestedPayload
	found := false
	for _, ev := range rec.events {
		if ev.Type != domain.EventApprovalRequested {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &requested); err != nil {
			t.Fatalf("解析 approval.requested: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("应当出现审批请求")
	}
	if requested.Path != "lib:b.txt" {
		t.Fatalf("审批路径应带别名：%q", requested.Path)
	}
	if !strings.Contains(requested.Diff, "lib:b.txt") {
		t.Fatalf("diff 头应带别名：%q", requested.Diff)
	}

	var writeAuditFound bool
	for _, tr := range rec.transitions {
		for _, rec := range tr.Audit {
			if rec.Action != "write.applied" {
				continue
			}
			writeAuditFound = true
			var detail map[string]any
			if err := json.Unmarshal(rec.Detail, &detail); err != nil {
				t.Fatalf("解析审计 detail: %v", err)
			}
			if detail["root"] != "lib" || detail["path"] != "lib:b.txt" {
				t.Fatalf("审计应记录根别名：%v", detail)
			}
		}
	}
	if !writeAuditFound {
		t.Fatal("应当有一条 write.applied 审计")
	}
}

// TestApproveWithoutRootsStillWritesPrimary 是回归：没挂附加根时行为不变。
func TestApproveWithoutRootsStillWritesPrimary(t *testing.T) {
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("c1", "out.txt", "hello\\n"),
		finalRound("写好了"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := mustRead(t, dir, "out.txt"); got != "hello\n" {
		t.Fatalf("主根内容不对：%q", got)
	}
}
