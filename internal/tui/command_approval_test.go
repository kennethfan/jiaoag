package tui

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

// 命令审批面板必须展示真实 argv、cwd、后端与超时，而不是一句「要不要执行命令」。
// 4 KB 上限的 token 不能把面板冲成一行：审批时要能读完整串。
func TestCommandApprovalPanelWrapsLongArgv(t *testing.T) {
	m := newModel(t)
	long := strings.Repeat("a", 300)
	m.handleEvent(eventEnvelope(t, 42, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_long",
		Kind:       domain.ApprovalCommand,
		Command:    []string{"cat", long},
		Cwd:        "/repo",
		Backend:    "direct",
		TimeoutMS:  120000,
	}))
	panel := m.renderApprovalPanel()

	var joined strings.Builder
	for _, line := range strings.Split(strings.TrimRight(panel, "\n"), "\n") {
		if n := len([]rune(line)); n > panelWidth {
			t.Fatalf("面板行 %d rune 超过宽度 %d：%q", n, panelWidth, line)
		}
		if strings.HasPrefix(line, "  ") {
			joined.WriteString(strings.TrimSpace(line))
		}
	}
	if !strings.Contains(joined.String(), long) {
		t.Fatal("折行后 token 内容不完整")
	}
}

// 命令失败/超时时，正文第一行（如「命令超时被终止」）必须出现在界面上：
// 摘要行只有退出码，看不出到底为什么失败。
func TestCommandResultShowsOutputPreview(t *testing.T) {
	m := newModel(t)
	code := -1
	m.handleEvent(eventEnvelope(t, 43, domain.EventToolResult, domain.ToolResultPayload{
		Tool:       "run_command",
		ToolCallID: "call_1",
		Status:     string(domain.ToolFailed),
		IsError:    true,
		ExitCode:   &code,
		Output:     "命令超时被终止（2m0s）",
		OutputPath: "/tmp/out/call_1.log",
	}))

	joined := strings.Join(m.dialog, "\n")
	if !strings.Contains(joined, "命令超时被终止") {
		t.Fatalf("界面应显示超时文案：%v", m.dialog)
	}
	if !strings.Contains(joined, "全文已落盘") {
		t.Fatalf("全文落盘提示丢失：%v", m.dialog)
	}
}

func TestCommandApprovalPanel(t *testing.T) {
	m := newModel(t)
	m.handleEvent(eventEnvelope(t, 40, domain.EventApprovalRequested, domain.ApprovalRequestedPayload{
		ApprovalID: "apr_cmd",
		Kind:       domain.ApprovalCommand,
		Command:    []string{"go", "test", "./..."},
		Cwd:        "/repo",
		Backend:    "direct",
		TimeoutMS:  120000,
		Reason:     "not_whitelisted: go 不在白名单内",
	}))
	if m.approval == nil || m.approval.kind != string(domain.ApprovalCommand) {
		t.Fatalf("面板状态 = %+v", m.approval)
	}

	panel := m.renderApprovalPanel()
	for _, want := range []string{
		"待审批命令", "argv[0]", "argv[1]", "go", "cwd", "/repo",
		"backend", "direct", "2m0s", "not_whitelisted", "[y] 批准一次",
	} {
		if !strings.Contains(panel, want) {
			t.Fatalf("命令面板缺少 %q：\n%s", want, panel)
		}
	}
	if strings.Contains(panel, "✎ 待审批") {
		t.Fatalf("命令审批不该走 diff 面板：\n%s", panel)
	}

	m.handleEvent(eventEnvelope(t, 41, domain.EventApprovalResolved, domain.ApprovalResolvedPayload{
		ApprovalID: "apr_cmd", Decision: domain.DecisionAllowOnce,
	}))
	if m.approval != nil {
		t.Fatal("对应裁决应关闭命令面板")
	}
}
