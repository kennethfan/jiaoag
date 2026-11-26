package tui

import (
	"strings"
	"testing"
)

// typeText 逐字符敲进输入行，走的是真实按键路径。
func typeText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		m.Update(keyMsg(string(r)))
	}
}

func TestSlashOpensCompletionList(t *testing.T) {
	m := newModel(t)
	typeText(t, m, "/")

	items := m.visibleCompletion()
	if len(items) != len(commandHints) {
		t.Fatalf("输入 / 应列出全部命令，得到 %d 条", len(items))
	}
	if items[0].name != "/models" {
		t.Fatalf("第一条应是 /models，得到 %s", items[0].name)
	}
	if got := m.renderCompletion(80); len(got) == 0 || !strings.Contains(got[0], "/models") {
		t.Fatalf("候选渲染异常：%v", got)
	}
}

func TestCompletionFiltersAndCompletes(t *testing.T) {
	m := newModel(t)
	typeText(t, m, "/mod")
	if items := m.visibleCompletion(); len(items) != 2 {
		t.Fatalf("/mod 应有两个候选，得到 %d", len(items))
	}

	m.Update(keyMsg("down"))
	m.Update(keyMsg("tab"))
	if m.input != "/model" {
		t.Fatalf("tab 补全后应是 /model，得到 %q", m.input)
	}
	// 打全之后浮层收起，下一次 enter 才是执行。
	if items := m.visibleCompletion(); len(items) != 0 {
		t.Fatalf("补全完成后浮层应收起，仍有 %d 条", len(items))
	}
}

func TestEnterCompletesPrefixButExecutesExactCommand(t *testing.T) {
	m := newModel(t)
	typeText(t, m, "/mod")

	_, cmd := m.Update(keyMsg("enter"))
	if cmd != nil {
		t.Fatal("前缀状态下的 enter 只应补全，不应执行")
	}
	if m.input != "/models" {
		t.Fatalf("应补全为 /models，得到 %q", m.input)
	}

	_, cmd = m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("完整命令名下的 enter 应执行 /models")
	}
	if m.input != "" {
		t.Fatalf("执行后输入行应清空，得到 %q", m.input)
	}

	// /model 是 /models 的前缀，但打全了就该按 /model 执行。
	m2 := newModel(t)
	typeText(t, m2, "/model")
	if _, cmd := m2.Update(keyMsg("enter")); cmd == nil || !m2.modelsForTask {
		t.Fatal("/model 打全后 enter 应打开单次模型选择")
	}
}

func TestEscClosesCompletionBeforeCancel(t *testing.T) {
	m := newModel(t)
	m.runTaskID = "tsk_1"
	typeText(t, m, "/mod")

	if _, cmd := m.Update(keyMsg("esc")); cmd != nil {
		t.Fatal("第一次 esc 只应关浮层，不应取消任务或退出")
	}
	if m.input != "/mod" {
		t.Fatalf("关浮层应保留输入，得到 %q", m.input)
	}
	if items := m.visibleCompletion(); len(items) != 0 {
		t.Fatal("esc 后浮层应关闭")
	}

	if _, cmd := m.Update(keyMsg("esc")); cmd == nil {
		t.Fatal("第二次 esc 应回到原有语义（取消运行中的任务）")
	}
}

func TestSpaceLeavesCommandMode(t *testing.T) {
	m := newModel(t)
	typeText(t, m, "/rename")
	m.Update(keyMsg("space"))
	if items := m.visibleCompletion(); len(items) != 0 {
		t.Fatalf("进入参数区后不应再展示候选，仍有 %d 条", len(items))
	}
}

func TestCompletionKeepsFrameHeight(t *testing.T) {
	m := newModel(t)
	m.width, m.height = 80, 20
	m.fullscreen = true
	for i := 0; i < 40; i++ {
		m.appendLine("历史输出")
	}
	typeText(t, m, "/")

	view := m.View().Content
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("帧高应恒等于终端高度 20，得到 %d", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "已连接") && !strings.Contains(lines[len(lines)-1], "未连接") {
		t.Fatalf("最后一行应是状态栏：%q", lines[len(lines)-1])
	}
	if !strings.Contains(lines[len(lines)-2], "› ") {
		t.Fatalf("倒数第二行应是输入行：%q", lines[len(lines)-2])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "/models") {
		t.Fatal("浮层里应能看到 /models")
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	m := newModel(t)
	rows := m.helpRows()
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		seen[r[0]] = true
	}
	for _, c := range commandHints {
		if !seen[c.name] {
			t.Fatalf("帮助面板缺少命令 %s", c.name)
		}
	}
}
