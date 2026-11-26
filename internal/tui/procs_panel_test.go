package tui

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

func procFrame(t *testing.T, procs ...protocol.ProcInfo) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(protocol.TypeProcListResult, protocol.ProcListResultPayload{Procs: procs})
	if err != nil {
		t.Fatalf("构造 proc.list.result: %v", err)
	}
	return env
}

func sampleProc(id, status, sessionID string) protocol.ProcInfo {
	return protocol.ProcInfo{
		ID:        id,
		SessionID: sessionID,
		Owner:     "rt_1",
		PID:       42,
		Argv:      []string{"npm", "run", "dev"},
		Status:    status,
	}
}

func openProcsPanel(t *testing.T, m *Model, procs ...protocol.ProcInfo) {
	t.Helper()
	m.handleEnvelope(procFrame(t, procs...))
	if m.procsPanel == nil {
		t.Fatal("proc.list.result 应该打开面板")
	}
}

func TestPSCommandAsksForList(t *testing.T) {
	m := newModel(t)
	typeText(t, m, "/ps")
	_, cmd := m.handleKey(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("应该发出 proc.list")
	}
	if m.procsPanel != nil {
		t.Fatal("还没拿到结果就开面板了")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "正在获取后台进程") {
		t.Fatalf("缺少提示：%v", m.dialog)
	}
}

func TestProcListResultOpensPanel(t *testing.T) {
	m := newModel(t)
	openProcsPanel(t, m,
		sampleProc("call_orphan", "orphaned", "ses_9"),
		sampleProc("call_run", "running", "ses_1"),
	)
	view := m.renderProcsPanel()
	for _, want := range []string{"后台进程（2 条", "孤儿", "[孤儿]", "[运行中]", "npm run dev", "pid 42"} {
		if !strings.Contains(view, want) {
			t.Fatalf("面板缺少 %q：\n%s", want, view)
		}
	}
}

func TestProcListEmptyDoesNotOpenPanel(t *testing.T) {
	m := newModel(t)
	m.handleEnvelope(procFrame(t))
	if m.procsPanel != nil {
		t.Fatal("空列表不该开面板")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "没有后台进程") {
		t.Fatalf("缺少空列表提示：%v", m.dialog)
	}
}

func TestProcCleanNeedsTwoPresses(t *testing.T) {
	m := newModel(t)
	openProcsPanel(t, m, sampleProc("call_exited", "exited", "ses_1"))

	_, cmd := m.handleProcsKey("d")
	if cmd != nil {
		t.Fatal("第一次按 d 只应该进入确认，不该发帧")
	}
	if m.procsPanel.confirm != "d" {
		t.Fatalf("应该进入删除确认，实际 %q", m.procsPanel.confirm)
	}
	if !strings.Contains(m.renderProcsPanel(), "再按一次") {
		t.Fatal("确认态应该有提示")
	}

	_, cmd = m.handleProcsKey("d")
	if cmd == nil {
		t.Fatal("第二次按 d 应该发出 proc.clean")
	}
	if m.procsPanel.confirm != "" {
		t.Fatal("确认后应该清掉状态")
	}
}

func TestProcCleanRefusesRunning(t *testing.T) {
	m := newModel(t)
	openProcsPanel(t, m, sampleProc("call_run", "running", "ses_1"))

	_, cmd := m.handleProcsKey("d")
	if cmd != nil {
		t.Fatal("运行中的进程不该删记录")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "先按 s 停止") {
		t.Fatalf("应该提示先停止：%v", m.dialog)
	}
}

func TestProcKillOnlyForOrphans(t *testing.T) {
	m := newModel(t)
	openProcsPanel(t, m, sampleProc("call_run", "running", "ses_1"))

	_, cmd := m.handleProcsKey("x")
	if cmd != nil {
		t.Fatal("运行中的进程不该被强制终止")
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "属主已失联") {
		t.Fatalf("应该解释只对孤儿生效：%v", m.dialog)
	}

	m2 := newModel(t)
	openProcsPanel(t, m2, sampleProc("call_orphan", "orphaned", "ses_9"))
	if _, cmd := m2.handleProcsKey("x"); cmd != nil {
		t.Fatal("第一次按 x 只应该进入确认")
	}
	if _, cmd := m2.handleProcsKey("x"); cmd == nil {
		t.Fatal("第二次按 x 应该发出 proc.kill")
	}
}

func TestProcPanelEscCloses(t *testing.T) {
	m := newModel(t)
	openProcsPanel(t, m, sampleProc("call_run", "running", "ses_1"))
	if _, cmd := m.handleProcsKey("esc"); cmd != nil {
		t.Fatal("esc 不该发帧")
	}
	if m.procsPanel != nil {
		t.Fatal("esc 应该关掉面板")
	}
}
