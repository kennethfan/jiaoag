package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/domain"
)

// jsonString 把普通文本转成可以嵌进 writeRound 的 JSON 字符串体。
// writeRound 直接把 content 拼进参数 JSON，所以换行必须是 \n 转义。
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b[1 : len(b)-1])
}

// twoHunkFixture 生成一份能被切成两个块的改动：第 2 行与第 18 行。
func twoHunkFixture(t *testing.T) (old, updated string) {
	t.Helper()
	var o, b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&o, "line %02d\n", i)
		switch i {
		case 2:
			b.WriteString("line 02 changed\n")
		case 18:
			b.WriteString("line 18 changed\n")
		default:
			fmt.Fprintf(&b, "line %02d\n", i)
		}
	}
	return o.String(), b.String()
}

// TestApplyOnlySelectedHunk 验证「只批第二个块」时磁盘上只出现那部分改动。
func TestApplyOnlySelectedHunk(t *testing.T) {
	old, updated := twoHunkFixture(t)
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "a.txt", jsonString(t, updated)),
		finalRound("只应用了一块"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.decide = func(agent.ApprovalRequest) (agent.ApprovalResult, error) {
		return agent.ApprovalResult{
			Decision: domain.DecisionAllowOnce,
			Source:   domain.SourceUser,
			Hunks:    []int{1}, // 只批第二个块
		}, nil
	}

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(updated, "line 02 changed", "line 02", 1)
	if string(got) != want {
		t.Fatalf("磁盘内容不对\n得到:\n%s\n期望:\n%s", got, want)
	}
	if !strings.Contains(string(got), "line 18 changed") {
		t.Fatalf("选中的块没有写入:\n%s", got)
	}

	// 工具结果要把「哪些块没写」告诉模型，否则它会以为整份改动都在盘上。
	result := lastToolResult(t, rec)
	if result.IsError {
		t.Fatalf("工具结果不应是错误：%+v", result)
	}
	for _, frag := range []string{"第 2 块", "第 1 块", "没有应用"} {
		if !strings.Contains(result.Output, frag) {
			t.Fatalf("工具结果缺少 %q：%s", frag, result.Output)
		}
	}

	// 事件要能让人看出这次只落了一部分。
	var applied *domain.ProposalAppliedPayload
	for _, ev := range rec.events {
		if ev.Type != domain.EventProposalApplied {
			continue
		}
		var p domain.ProposalAppliedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("解 proposal.applied: %v", err)
		}
		applied = &p
	}
	if applied == nil {
		t.Fatal("没有 proposal.applied 事件")
	}
	if len(applied.Hunks) != 1 || applied.Hunks[0] != 1 || applied.TotalHunks != 2 {
		t.Fatalf("proposal.applied 的块信息不对：%+v", applied)
	}

	// 审计要标出这是部分应用，并记下写了哪些块。
	audits := recordedAudits(rec)
	var write *domain.AuditRecord
	for i := range audits {
		if audits[i].Action == "write.applied" {
			write = &audits[i]
		}
	}
	if write == nil {
		t.Fatalf("没有 write.applied 审计：%+v", audits)
	}
	var detail map[string]any
	if err := json.Unmarshal(write.Detail, &detail); err != nil {
		t.Fatalf("解审计 detail: %v", err)
	}
	if detail["partial"] != true {
		t.Fatalf("审计未标记 partial：%s", write.Detail)
	}
	if detail["total_hunks"] != float64(2) {
		t.Fatalf("审计里的块总数不对：%s", write.Detail)
	}
}

// TestApprovalWithoutHunksWritesWholeChange 确认省略 hunks 时行为不变。
func TestApprovalWithoutHunksWritesWholeChange(t *testing.T) {
	_, updated := twoHunkFixture(t)
	rec := &fakeRecorder{}
	provider := &scriptedProvider{scripts: []streamScript{
		writeRound("call_w", "a.txt", jsonString(t, updated)),
		finalRound("全量"),
	}}
	ag, task, dir := newWriteAgent(t, provider, rec, 4)

	if err := ag.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != updated {
		t.Fatalf("省略 hunks 时应整份写入\n得到:\n%s\n期望:\n%s", got, updated)
	}
	for _, ev := range rec.events {
		if ev.Type != domain.EventProposalApplied {
			continue
		}
		var p domain.ProposalAppliedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("解 proposal.applied: %v", err)
		}
		if len(p.Hunks) != 0 || p.TotalHunks != 0 {
			t.Fatalf("整份应用不应带块信息：%+v", p)
		}
	}
}
