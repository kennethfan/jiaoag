package prompt_test

import (
	"testing"

	"github.com/kennethfan/jiaoag/internal/prompt"
)

func vars() map[string]string {
	return map[string]string{
		"workspace": "/tmp/ws",
		"os":        "darwin",
		"shell":     "zsh",
		"date":      "2026-11-11",
		"model":     "test-model",
		"tools":     "- read_file：读取",
	}
}

func TestAssembleSubstitutesAllVars(t *testing.T) {
	layer := prompt.Layer{Source: "test", Text: "ws={{workspace}} os={{os}} model={{model}}"}
	result, err := prompt.Assemble([]prompt.Layer{layer}, vars())
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if result.Text != "ws=/tmp/ws os=darwin model=test-model" {
		t.Fatalf("拼装结果不对：%q", result.Text)
	}
	if result.Hash == "" {
		t.Fatal("Hash 不应为空")
	}
}

func TestAssembleRejectsUnknownTemplateVar(t *testing.T) {
	layer := prompt.Layer{Source: "test", Text: "{{unknown}}"}
	if _, err := prompt.Assemble([]prompt.Layer{layer}, vars()); err == nil {
		t.Fatal("未定义的模板变量应报错")
	}
}

func TestAssembleRejectsUnknownProvidedVar(t *testing.T) {
	v := vars()
	v["extra"] = "x"
	if _, err := prompt.Assemble([]prompt.Layer{prompt.Builtin()}, v); err == nil {
		t.Fatal("未定义的注入变量应报错")
	}
}

func TestAssembleRejectsMissingValue(t *testing.T) {
	v := vars()
	delete(v, "model")
	if _, err := prompt.Assemble([]prompt.Layer{prompt.Builtin()}, v); err == nil {
		t.Fatal("缺少变量值应报错")
	}
}

func TestHashIgnoresDateButTracksWorkspace(t *testing.T) {
	layers := []prompt.Layer{prompt.Builtin()}

	first := vars()
	second := vars()
	second["date"] = "2027-01-01"
	if prompt.Hash(layers, first) != prompt.Hash(layers, second) {
		t.Fatal("日期变化不应改变提示词指纹")
	}

	third := vars()
	third["workspace"] = "/tmp/other"
	if prompt.Hash(layers, first) == prompt.Hash(layers, third) {
		t.Fatal("工作区变化应改变提示词指纹")
	}
}

func TestBuiltinContainsWorkspaceAndTools(t *testing.T) {
	result, err := prompt.Assemble([]prompt.Layer{prompt.Builtin()}, vars())
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for _, want := range []string{"/tmp/ws", "read_file", "简体中文"} {
		if !contains(result.Text, want) {
			t.Fatalf("内置提示词缺少 %q", want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
