package agent_test

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/agent"
	"github.com/kennethfan/jiaoag/internal/llm"
)

func TestBuildDropsOldestRoundFirst(t *testing.T) {
	history := []llm.Message{
		llm.TextMessage(llm.RoleUser, strings.Repeat("旧", 50)),
		llm.TextMessage(llm.RoleAssistant, strings.Repeat("答", 50)),
		llm.TextMessage(llm.RoleUser, "新问题"),
	}

	out := agent.Build(history, 10)
	if len(out) != 1 {
		t.Fatalf("应只保留最后一轮，实际 %d 条：%+v", len(out), out)
	}
	if out[0].Blocks[0].Text != "新问题" {
		t.Fatalf("保留的不是最后一条 user：%+v", out[0])
	}
}

func TestBuildKeepsLastUserMessageEvenOverBudget(t *testing.T) {
	history := []llm.Message{llm.TextMessage(llm.RoleUser, strings.Repeat("长", 100))}
	out := agent.Build(history, 10)
	if len(out) != 1 {
		t.Fatalf("最后一条 user 永不丢弃，实际 %d 条", len(out))
	}
}

func TestBuildKeepsEverythingUnderBudget(t *testing.T) {
	history := []llm.Message{
		llm.TextMessage(llm.RoleUser, "问题"),
		llm.TextMessage(llm.RoleAssistant, "回答"),
	}
	out := agent.Build(history, 1000)
	if len(out) != 2 {
		t.Fatalf("预算充足时不应裁剪，实际 %d 条", len(out))
	}
}

func TestBuildTruncatesLongToolResult(t *testing.T) {
	long := strings.Repeat("x", 20000)
	history := []llm.Message{
		llm.TextMessage(llm.RoleUser, "问题"),
		llm.ToolResultMessage(llm.Block{
			Kind:       llm.BlockToolResult,
			ToolCallID: "call_1",
			ToolName:   "read_file",
			Result:     long,
		}),
	}

	out := agent.Build(history, 1_000_000)
	result := out[1].Blocks[0].Result
	if len(result) >= len(long) {
		t.Fatalf("超长工具结果未被截断，长度 %d", len(result))
	}
	if !strings.Contains(result, "已截断") {
		t.Fatalf("截断后应带提示：%s", result[len(result)-100:])
	}
	if !strings.HasPrefix(result, strings.Repeat("x", 100)) {
		t.Fatal("截断后应保留头部")
	}
}

func TestBuildDoesNotMutateInput(t *testing.T) {
	long := strings.Repeat("y", 20000)
	history := []llm.Message{
		llm.ToolResultMessage(llm.Block{Kind: llm.BlockToolResult, Result: long}),
	}
	agent.Build(history, 1_000_000)
	if len(history[0].Blocks[0].Result) != len(long) {
		t.Fatal("Build 不应修改入参")
	}
}
