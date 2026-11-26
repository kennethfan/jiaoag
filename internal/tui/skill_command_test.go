package tui

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// manualSkillCatalog 造一份技能清单：一个仅手动、一个可被模型自选。
func manualSkillCatalog() []protocol.SkillInfo {
	return []protocol.SkillInfo{
		{Name: "grilling", Description: "反复追问，直到方案站得住", Manual: true},
		{Name: "auto-skill", Description: "模型可以自己挑", Manual: false},
	}
}

// 仅手动技能要进斜杠补全：它们本来就是「只能用户点名」的那一类。
func TestManualSkillAppearsInCompletion(t *testing.T) {
	m := newModel(t)
	m.skillCatalog = manualSkillCatalog()

	typeText(t, m, "/gri")
	items := m.visibleCompletion()
	if len(items) != 1 {
		t.Fatalf("/gri 应只剩一个技能候选，得到 %d 条：%+v", len(items), items)
	}
	if items[0].name != "/grilling" || items[0].tag != "技能" {
		t.Fatalf("候选应是带「技能」标记的 /grilling，得到 %+v", items[0])
	}
	if got := m.renderCompletion(100); len(got) == 0 || !strings.Contains(got[0], "技能") {
		t.Fatalf("渲染里应标出来源：%v", got)
	}

	m.Update(keyMsg("tab"))
	if m.input != "/grilling" {
		t.Fatalf("tab 应补全成 /grilling，得到 %q", m.input)
	}
}

// 非 manual 的技能不能变成命令：它们仍由模型通过 skill 工具自选。
func TestModelSelectableSkillIsNotACommand(t *testing.T) {
	m := newModel(t)
	m.skillCatalog = manualSkillCatalog()

	typeText(t, m, "/auto")
	if items := m.visibleCompletion(); len(items) != 0 {
		t.Fatalf("仅模型可选的技能不该进补全，得到 %d 条", len(items))
	}
}

// 带问题的一条输入应直接提交，且只加载这一条技能。
func TestSkillCommandSubmitsPromptDirectly(t *testing.T) {
	m := newModel(t)
	m.skillCatalog = manualSkillCatalog()
	m.input = "/grilling 这个设计该怎么拆"

	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("应发出 task.submit")
	}
	if m.input != "" {
		t.Fatalf("提交后输入行应清空，得到 %q", m.input)
	}
	// 一次性语义：提交后不能再粘在下一条任务上。
	if len(m.forcedSkills) != 0 {
		t.Fatalf("提交后技能应清空，得到 %v", m.forcedSkills)
	}

	name, rest, ok := m.skillCommand("/grilling 这个设计该怎么拆")
	if !ok || name != "grilling" || rest != "这个设计该怎么拆" {
		t.Fatalf("解析结果不对：%q %q %v", name, rest, ok)
	}
}

// 不带问题时先武装，不提交空任务。
func TestSkillCommandWithoutArgsArms(t *testing.T) {
	m := newModel(t)
	m.skillCatalog = manualSkillCatalog()
	m.input = "/grilling"

	if _, cmd := m.Update(keyMsg("enter")); cmd != nil {
		t.Fatal("不带问题时不该提交任务")
	}
	if len(m.forcedSkills) != 1 || m.forcedSkills[0] != "grilling" {
		t.Fatalf("应武装 grilling，得到 %v", m.forcedSkills)
	}
	if !strings.Contains(strings.Join(m.dialog, "\n"), "已指定技能 grilling") {
		t.Fatal("应提示已指定技能")
	}
}

// 重名时内置命令赢：技能目录归用户，不能顶掉 /model 这类真命令。
func TestBuiltinCommandWinsSkillCollision(t *testing.T) {
	m := newModel(t)
	m.skillCatalog = []protocol.SkillInfo{
		{Name: "model", Description: "假装成命令的技能", Manual: true},
	}
	m.input = "/model agnes-2.5-pro"

	if _, cmd := m.Update(keyMsg("enter")); cmd != nil {
		t.Fatal("内置 /model 带参数只应武装，不该发请求")
	}
	if m.forcedModel != "agnes-2.5-pro" {
		t.Fatalf("应走内置 /model，得到 %q", m.forcedModel)
	}
	if len(m.forcedSkills) != 0 {
		t.Fatalf("不该把重名技能当命令，得到 %v", m.forcedSkills)
	}
	if _, _, ok := m.skillCommand("/model agnes-2.5-pro"); ok {
		t.Fatal("重名技能不该被识别成技能命令")
	}
}

// skill.list 的结果要缓存下来供补全用；属于别的会话的结果不能混进来。
func TestSkillListResultCachesCatalog(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeSkillListResult, protocol.SkillListResultPayload{
		Skills: manualSkillCatalog(),
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	env.SessionID = "ses_1"
	m.handleEnvelope(env)
	if len(m.skillCatalog) != 2 || m.skillCatalogSession != "ses_1" {
		t.Fatalf("应缓存当前会话的技能清单，得到 %+v / %q", m.skillCatalog, m.skillCatalogSession)
	}

	other, err := protocol.New(protocol.TypeSkillListResult, protocol.SkillListResultPayload{
		Skills: []protocol.SkillInfo{{Name: "other", Manual: true}},
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	other.SessionID = "ses_2"
	m.handleEnvelope(other)
	if len(m.skillCatalog) != 2 {
		t.Fatalf("别的会话的清单不该覆盖当前缓存，得到 %+v", m.skillCatalog)
	}
}

// 全量补全里，内置命令在前、技能在后。
func TestCompletionListsBuiltinsBeforeSkills(t *testing.T) {
	m := newModel(t)
	m.skillCatalog = manualSkillCatalog()
	typeText(t, m, "/")

	items := m.visibleCompletion()
	if len(items) != len(commandHints)+1 {
		t.Fatalf("应列出内置命令 + 1 个技能，得到 %d 条", len(items))
	}
	if items[0].name != commandHints[0].name {
		t.Fatalf("第一条应是内置命令 %s，得到 %s", commandHints[0].name, items[0].name)
	}
	last := items[len(items)-1]
	if last.name != "/grilling" || last.tag != "技能" {
		t.Fatalf("末尾应是技能，得到 %+v", last)
	}
}

// 会话建立时的自动拉取只更新缓存，不能弹面板盖住对话。
func TestAutoSkillFetchDoesNotOpenPanel(t *testing.T) {
	m := newModel(t)
	env, err := protocol.New(protocol.TypeSkillListResult, protocol.SkillListResultPayload{
		Skills: manualSkillCatalog(),
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	env.SessionID = "ses_1"
	m.handleEnvelope(env)
	if m.skillPanel != nil {
		t.Fatal("自动拉取清单不该打开技能面板")
	}
	if len(m.skillCatalog) != 2 {
		t.Fatalf("仍应缓存清单，得到 %d 条", len(m.skillCatalog))
	}

	// 用户显式 /skill 时照旧开面板。
	m.input = "/skill"
	m.Update(keyMsg("enter"))
	env2, err := protocol.New(protocol.TypeSkillListResult, protocol.SkillListResultPayload{
		Skills: manualSkillCatalog(),
	})
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	env2.SessionID = "ses_1"
	m.handleEnvelope(env2)
	if m.skillPanel == nil {
		t.Fatal("显式 /skill 应打开面板")
	}
}
