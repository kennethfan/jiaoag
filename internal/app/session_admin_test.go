package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// 会话管理面板的三个动作：列表、改名、删除。
func TestSessionManageListRenameDelete(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	dir := t.TempDir()

	keep, err := a.CreateSession(ctx, dir, "openai", "gpt-x", "")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	doomed, err := a.CreateSession(ctx, dir, "openai", "gpt-x", "待删")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	// 别的目录的会话不该出现在这个工作区的列表里。
	if _, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "别处"); err != nil {
		t.Fatalf("创建别处会话: %v", err)
	}
	if _, err := a.SubmitTask(ctx, keep.ID, "统计一条任务"); err != nil {
		t.Fatalf("提交任务: %v", err)
	}

	summaries, err := a.SessionSummaries(ctx, dir)
	if err != nil {
		t.Fatalf("列会话: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("同工作区应有 2 条，得到 %d", len(summaries))
	}
	counts := map[domain.SessionID]int{}
	for _, sum := range summaries {
		counts[sum.ID] = sum.Tasks
	}
	if counts[keep.ID] != 1 {
		t.Fatalf("任务数 = %d", counts[keep.ID])
	}

	// 改名：首尾空白去掉，回放里能看到 session.renamed。
	renamed, err := a.RenameSession(ctx, keep.ID, "  给标题留个记号  ")
	if err != nil {
		t.Fatalf("改名: %v", err)
	}
	if renamed.Title != "给标题留个记号" {
		t.Fatalf("标题 = %q", renamed.Title)
	}
	events, err := a.Replay(ctx, keep.ID, 0, 0)
	if err != nil {
		t.Fatalf("回放: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != domain.EventSessionRenamed {
		t.Fatalf("最后一条应为 session.renamed，得到 %s", last.Type)
	}
	var payload domain.SessionRenamedPayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("解载荷: %v", err)
	}
	if payload.Title != "给标题留个记号" {
		t.Fatalf("事件标题 = %q", payload.Title)
	}

	// 空标题表示清掉，不报错。
	if cleared, err := a.RenameSession(ctx, keep.ID, "   "); err != nil || cleared.Title != "" {
		t.Fatalf("清空标题: %q %v", cleared.Title, err)
	}
	if _, err := a.RenameSession(ctx, keep.ID, strings.Repeat("长", 61)); err == nil {
		t.Fatal("超长标题应报错")
	}

	// 删除：没有未结束任务（上面任务是 queued，算活跃）→ 先断言拒绝，再换一条。
	if err := a.DeleteSession(ctx, keep.ID); !errors.Is(err, domain.ErrSessionBusy) {
		t.Fatalf("排队任务的会话应拒绝删除，得到 %v", err)
	}
	if err := a.DeleteSession(ctx, doomed.ID); err != nil {
		t.Fatalf("删除空闲会话: %v", err)
	}
	if _, err := a.GetSession(ctx, doomed.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("会话应已删除，得到 %v", err)
	}
}
