package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
)

// twoHunkProvider 让模型把文件的两处（第 2 行与第 18 行）一起改掉，
// 从而生成两个 hunk，供「只批其中一块」的用例使用。
type twoHunkProvider struct{ wrote bool }

func (p *twoHunkProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试只走流式")
}

func (p *twoHunkProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if p.wrote {
		if err := fn(llm.Chunk{TextDelta: "改好了"}); err != nil {
			return err
		}
		return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd})
	}
	p.wrote = true
	if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{
		Index: 0, ID: "call_two", Name: "write_file",
	}}); err != nil {
		return err
	}
	content := "line 01\\nline 02 changed\\nline 03\\nline 04\\nline 05\\nline 06\\nline 07\\n" +
		"line 08\\nline 09\\nline 10\\nline 11\\nline 12\\nline 13\\nline 14\\nline 15\\n" +
		"line 16\\nline 17\\nline 18 changed\\nline 19\\nline 20\\n"
	if err := fn(llm.Chunk{ToolCallDelta: &llm.ToolCallDelta{
		Index: 0, ArgsFragment: `{"path":"a.txt","content":"` + content + `"}`,
	}}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopToolUse})
}

func twoHunkBase() string {
	out := ""
	for i := 1; i <= 20; i++ {
		out += "line " + pad2(i) + "\n"
	}
	return out
}

func pad2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return "1" + string(rune('0'+i-10))
}

// TestApprovalResolvePassesHunks 验证 approval.resolve 里的 hunks 一路传到
// agent：只写选中的块，并把块信息记进 proposal.applied。
func TestApprovalResolvePassesHunks(t *testing.T) {
	ts, application := newLedgerServer(t)
	configureWriteRuntime(t, application, &twoHunkProvider{})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(twoHunkBase()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := application.CreateSession(ctx, dir, "openai", "test-model", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	if _, err := application.SubmitTask(ctx, sess.ID, "改两处"); err != nil {
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

	requested := awaitEvent(t, conn, domain.EventApprovalRequested)
	approval := decodeEvent[domain.ApprovalRequestedPayload](t, requested)

	// 只批第二个块（下标 1）。
	resolve, err := protocol.New(protocol.TypeApprovalResolve, protocol.ApprovalResolvePayload{
		SessionID:  string(sess.ID),
		ApprovalID: string(approval.ApprovalID),
		Decision:   string(domain.DecisionAllowOnce),
		Hunks:      []int{1},
	})
	if err != nil {
		t.Fatalf("构造 approval.resolve: %v", err)
	}
	write(t, conn, resolve)

	applied := awaitEvent(t, conn, domain.EventProposalApplied)
	payload := decodeEvent[domain.ProposalAppliedPayload](t, applied)
	if len(payload.Hunks) != 1 || payload.Hunks[0] != 1 || payload.TotalHunks != 2 {
		t.Fatalf("proposal.applied 的块信息不对：%+v", payload)
	}

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "line 01\nline 02\nline 03\nline 04\nline 05\nline 06\nline 07\n" +
		"line 08\nline 09\nline 10\nline 11\nline 12\nline 13\nline 14\nline 15\n" +
		"line 16\nline 17\nline 18 changed\nline 19\nline 20\n"
	if string(got) != want {
		t.Fatalf("磁盘内容不对\n得到:\n%s\n期望:\n%s", got, want)
	}
}
