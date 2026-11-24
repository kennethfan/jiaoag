package tui

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// shell 形式的命令必须按「一条整命令」展示：拆成 argv[0]=sh、argv[1]=-c
// 会把真正要读的那段推到第三行，还会让人以为命令是 sh 而不是里面的脚本。
func TestCommandApprovalPanelShowsShellLine(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 42, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_shell",
		Kind:       domain.ApprovalCommand,
		Command:    []string{"sh", "-c", "go test ./... 2>&1 | tail -20"},
		Shell:      "go test ./... 2>&1 | tail -20",
		Cwd:        "/repo",
		Backend:    "direct",
		TimeoutMS:  120000,
		Reason:     "shell: 需要 shell 解释（管道/重定向/多命令），无法静态判定",
	}))

	panel := m.renderApprovalPanel()
	flat := stripANSI(panel)
	if strings.Contains(flat, "argv[0]") {
		t.Fatalf("shell 命令不应拆成 argv：%s", flat)
	}
	if !strings.Contains(flat, "shell") || !strings.Contains(flat, "go test ./... 2>&1 | tail -20") {
		t.Fatalf("面板缺少整条 shell 命令：%s", flat)
	}
	if !strings.Contains(flat, "需要 shell 解释") {
		t.Fatalf("面板缺少判定理由：%s", flat)
	}
}

// 对话区那一行提示也应该显示整条命令，而不是 sh -c <整条>。
func TestCommandApprovalAnnounceUsesShellText(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 42, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_shell",
		Kind:       domain.ApprovalCommand,
		Command:    []string{"sh", "-c", "ls | head -3"},
		Shell:      "ls | head -3",
		Cwd:        "/repo",
	}))

	flat := strings.Join(m.dialog, "\n")
	if strings.Contains(flat, "sh -c") {
		t.Fatalf("提示行不应带 sh -c 前缀：%s", flat)
	}
	if !strings.Contains(flat, "ls | head -3") {
		t.Fatalf("提示行缺少命令正文：%s", flat)
	}
}
