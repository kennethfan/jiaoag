package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/localshell"
)

func pressEnter(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return cmd
}

// drainLocal 把本地命令的事件读完，返回输出行与结束态。
func drainLocal(t *testing.T, run *localshell.Run) ([]string, localshell.Event) {
	t.Helper()
	var lines []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				t.Fatal("channel 在 Done 之前关闭")
			}
			if ev.Done {
				return lines, ev
			}
			lines = append(lines, ev.Text)
		case <-deadline:
			t.Fatal("等本地命令结束超时")
		}
	}
}

func TestLocalCommandRunsWithoutSession(t *testing.T) {
	m := New(Options{Workspace: t.TempDir()})
	m.sessionID = "" // 服务端没连上时也应该能跑本地命令
	m.input = "!echo hi"
	cmd := pressEnter(t, m)

	if cmd == nil {
		t.Fatal("!命令 应返回执行用的 cmd")
	}
	if m.input != "" {
		t.Fatalf("输入行未清空：%q", m.input)
	}
	if m.local == nil {
		t.Fatal("应记录运行中的本地命令")
	}
	t.Cleanup(m.local.Stop)
	if len(m.dialog) == 0 || m.dialog[0] != "! echo hi" {
		t.Fatalf("对话区应先回显命令：%v", m.dialog)
	}
}

func TestLocalCommandEmptyShowsHint(t *testing.T) {
	m := New(Options{Workspace: t.TempDir()})
	m.input = "!"
	pressEnter(t, m)

	if m.local != nil {
		t.Fatal("空命令不应启动进程")
	}
	if !hasLine(m.dialog, "后面要跟命令") {
		t.Fatalf("应提示用法：%v", m.dialog)
	}
}

func TestLocalCommandOutputIsIndentedAndWrapped(t *testing.T) {
	m := New(Options{Workspace: t.TempDir()})
	long := strings.Repeat("x", panelWidth+10)

	m.handleLocalCommand(localshell.Event{Text: "hi"})
	m.handleLocalCommand(localshell.Event{Text: long})

	if len(m.dialog) < 3 {
		t.Fatalf("长行应折成多行：%v", m.dialog)
	}
	for _, line := range m.dialog {
		if !strings.HasPrefix(line, outputIndent) {
			t.Fatalf("输出行应带缩进：%q", line)
		}
		if n := len([]rune(line)); n > panelWidth {
			t.Fatalf("输出行超宽（%d）：%q", n, line)
		}
	}
	if m.localLines != 2 {
		t.Fatalf("渲染行计数 = %d，期望 2", m.localLines)
	}
}

func TestLocalCommandDoneLineReportsExitCode(t *testing.T) {
	m := New(Options{Workspace: t.TempDir()})
	m.handleLocalCommand(localshell.Event{Done: true, ExitCode: 1, Duration: 1500 * time.Millisecond})

	last := m.dialog[len(m.dialog)-1]
	if !strings.Contains(last, "退出码 1") || !strings.Contains(last, "1.5s") {
		t.Fatalf("结束行 = %q", last)
	}
}

func TestLocalCommandOutputIsCapped(t *testing.T) {
	m := New(Options{Workspace: t.TempDir()})
	m.localLines = localCommandLines

	m.handleLocalCommand(localshell.Event{Text: "看不见的这一行"})
	if m.localOmitted != 1 {
		t.Fatalf("省略计数 = %d，期望 1", m.localOmitted)
	}
	if hasLine(m.dialog, "看不见的这一行") {
		t.Fatalf("超过上限后不应渲染：%v", m.dialog)
	}

	m.handleLocalCommand(localshell.Event{Done: true, ExitCode: 0, Duration: 10 * time.Millisecond})
	last := m.dialog[len(m.dialog)-1]
	if !strings.Contains(last, "省略 1 行") {
		t.Fatalf("结束行应说明省略了多少行：%q", last)
	}
	if m.localLines != 0 || m.localOmitted != 0 {
		t.Fatalf("结束时应重置计数：%d/%d", m.localLines, m.localOmitted)
	}
}

func TestCtrlXStopsLocalCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实进程")
	}
	m := New(Options{Workspace: t.TempDir()})
	m.local = localshell.Start(localshell.Options{Workdir: t.TempDir(), Timeout: 30 * time.Second}, "sleep 5")

	m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	if !hasLine(m.dialog, "正在终止本地命令") {
		t.Fatalf("应提示正在终止：%v", m.dialog)
	}
	_, done := drainLocal(t, m.local)
	if !done.Stopped {
		t.Fatalf("结束态应标记为已终止：%+v", done)
	}
	m.handleLocalCommand(done)
	if last := m.dialog[len(m.dialog)-1]; !strings.Contains(last, "已终止") {
		t.Fatalf("结束行 = %q", last)
	}
}

func hasLine(lines []string, sub string) bool {
	for _, line := range lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}
