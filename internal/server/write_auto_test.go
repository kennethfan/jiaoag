package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// configureAutoWriteRuntime 与 configureWriteRuntime 同形，但打开工作区免审写入。
func configureAutoWriteRuntime(t *testing.T, a *app.App, provider llm.Provider) {
	t.Helper()
	err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return provider, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewWriteFile(ws), tools.NewReadFile(ws))
		},
		MaxRounds:     4,
		ContextBudget: 10000,
		WriteApproval: "workspace",
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
}

// 工作区内的写入不产生审批，任务直接完成，并把 auto 标记写进事件。
// 这个用例同时守着「免审路径不能写 running→running 的非法迁移」。
func TestWorkspaceWriteCompletesWithoutApproval(t *testing.T) {
	ts, application := newLedgerServer(t)
	configureAutoWriteRuntime(t, application, &writeOnceProvider{})

	dir := t.TempDir()
	ctx := context.Background()
	sess, err := application.CreateSession(ctx, dir, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if _, err := application.SubmitTask(ctx, sess.ID, "写文件"); err != nil {
		t.Fatalf("提交任务: %v", err)
	}

	conn := dial(t, wsURL(ts), testToken)
	handshake(t, conn)
	sub, err := protocol.New(protocol.TypeEventSubscribe, protocol.EventSubscribePayload{
		SessionID: string(sess.ID),
		FromSeq:   0,
	})
	if err != nil {
		t.Fatalf("构造 event.subscribe: %v", err)
	}
	write(t, conn, sub)

	applied := decodeEvent[domain.ProposalAppliedPayload](t, awaitEvent(t, conn, domain.EventProposalApplied))
	if !applied.Auto || applied.Mode != "workspace" {
		t.Fatalf("proposal.applied 应标记 auto/workspace：%+v", applied)
	}
	status := decodeEvent[domain.TaskStatusPayload](t, awaitEvent(t, conn, domain.EventTaskStatus))
	for status.To != domain.TaskCompleted {
		if status.To.Terminal() {
			t.Fatalf("任务没有完成：%+v", status)
		}
		status = decodeEvent[domain.TaskStatusPayload](t, awaitEvent(t, conn, domain.EventTaskStatus))
	}

	// writeOnceProvider 写的是 out.txt（见 approval_test.go）。
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}
}
