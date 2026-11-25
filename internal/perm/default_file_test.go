package perm_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/perm"
)

// writeRules 在临时目录里落一份规则文件。测试自己在首次 Load 之前写，
// 这样不会触发「生成默认规则」的逻辑。
func writeRules(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("写 %s: %v", name, err)
	}
}

func TestLoaderGeneratesDefaultRulesOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "policy.d")

	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("首次加载: %v", err)
	}
	path := filepath.Join(dir, "default.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("默认规则应被生成: %v", err)
	}
	if !strings.Contains(string(body), "version: builtin-4") {
		t.Fatalf("生成的默认规则应带模板版本，实际前 80 字节: %.80s", body)
	}
	if len(rs.Warnings) == 0 || !strings.Contains(strings.Join(rs.Warnings, "\n"), "已生成默认规则") {
		t.Fatalf("首次生成应给出提示，实际 %v", rs.Warnings)
	}
	// 生成出来的文件必须真的生效。
	if d := rs.Evaluate(context.Background(), commandPrepared("go", "build", "./...")); d.Verdict != perm.Allow {
		t.Fatalf("生成的默认规则应放行 go build，实际 %q（%s）", d.Verdict, d.Reason)
	}

	// 第二次加载：不覆盖、不重复提示。
	custom := "version: builtin-4\nrules: []\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatalf("改写默认规则: %v", err)
	}
	rs2, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("二次加载: %v", err)
	}
	if len(rs2.Warnings) != 0 {
		t.Fatalf("已存在的默认规则不应再提示生成: %v", rs2.Warnings)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回默认规则: %v", err)
	}
	if string(after) != custom {
		t.Fatalf("默认规则被覆盖了：%s", after)
	}
	if d := rs2.Evaluate(context.Background(), commandPrepared("go", "build", "./...")); d.Verdict == perm.Allow {
		t.Fatalf("清空规则后不该再放行 go build")
	}
}

func TestDefaultFileCanAllowButOtherFilesCannot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "policy.d")
	writeRules(t, dir, "default.yaml", `
version: test-allow
rules:
  - id: exec.python
    programs: [python3]
    allow_flags: ["-v"]
`)
	// 另一个文件里的同名程序规则只能收紧，不能放行。
	writeRules(t, dir, "99-local.yaml", `
version: local-1
rules:
  - id: local.kubectl
    programs: [kubectl]
`)
	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("加载: %v", err)
	}

	if d := rs.Evaluate(context.Background(), commandPrepared("python3", "-v")); d.Verdict != perm.Allow {
		t.Fatalf("default.yaml 应能放宽 python3，实际 %q（%s）", d.Verdict, d.Reason)
	}
	if d := rs.Evaluate(context.Background(), commandPrepared("python3", "-c", "print(1)")); d.Verdict == perm.Allow {
		t.Fatalf("未列出的 flag 不该放行")
	}
	if d := rs.Evaluate(context.Background(), commandPrepared("kubectl", "get", "pods")); d.Verdict == perm.Allow {
		t.Fatalf("policy.d 其他文件只能收紧，不能放宽，实际放行了")
	}
}

func TestBuiltinRiskFloorSurvivesEmptyRuleList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "policy.d")
	writeRules(t, dir, "default.yaml", `
version: test-risk
rules:
  - id: exec.npm
    programs: [npm, pnpm]
`)
	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	d := rs.Evaluate(context.Background(), commandPrepared("npm", "publish"))
	if d.Verdict != perm.RequireUser || !strings.Contains(d.Reason, "risky_subcommand") {
		t.Fatalf("publish 必须落到审批，实际 %q（%s）", d.Verdict, d.Reason)
	}
	if d := rs.Evaluate(context.Background(), commandPrepared("npm", "run", "build")); d.Verdict != perm.Allow {
		t.Fatalf("npm run 应放行，实际 %q（%s）", d.Verdict, d.Reason)
	}
}

func TestDefaultVersionMismatchWarns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "policy.d")
	writeRules(t, dir, "default.yaml", `
version: builtin-0
rules: []
`)
	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("加载: %v", err)
	}
	if !strings.Contains(strings.Join(rs.Warnings, "\n"), "内置模板") {
		t.Fatalf("版本不同应提示，实际 %v", rs.Warnings)
	}
}
