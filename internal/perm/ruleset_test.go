package perm_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
)

func commandPrepared(argv ...string) tools.Prepared {
	return tools.Prepared{
		Call:    tools.Call{Name: "run_command"},
		Spec:    tools.Spec{Name: "run_command", Effects: []tools.Effect{tools.EffectProcess}},
		Command: &tools.Command{Argv: argv, Cwd: "/tmp"},
	}
}

func TestWhitelistAllowsSafeArgv(t *testing.T) {
	rs := perm.DefaultRuleset()
	cases := [][]string{
		{"go", "test", "./..."},
		{"go", "vet", "./..."},
		{"go", "run", "./cmd/app"},
		{"ls", "-la"},
		{"cat", "a.txt"},
		{"cat", "-n", "a.txt"},
	}
	for _, argv := range cases {
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.Allow {
			t.Fatalf("%v 应自动放行，实际 %q（%s）", argv, d.Verdict, d.Reason)
		}
		if d.PolicyVersion == "" || d.RuleID == "" {
			t.Fatalf("%v 的判定应带规则版本与 id：%+v", argv, d)
		}
	}
}

// 白名单只认程序名本身，不认 basename：带路径的 argv[0] 执行的是工作区或
// 任意路径上的程序，那不是「命中白名单」能担保的东西。
func TestWhitelistRejectsPathQualifiedProgram(t *testing.T) {
	rs := perm.DefaultRuleset()
	cases := [][]string{
		{"./go", "test", "./..."},
		{"/usr/bin/go", "vet", "./..."},
		{"../bin/ls", "-la"},
		{"/tmp/x/cat", "a.txt"},
	}
	for _, argv := range cases {
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("%v 应要求审批，实际 %q（%s）", argv, d.Verdict, d.Reason)
		}
	}
}

// go build 会把产物写进当前目录（`go build ./...` 也会为每个 main 包落一个），
// 绕过「写入必须审批」；git 读命令会执行仓库配置里的外部程序，统一收进审批。
func TestWorkspaceWritingOrConfigDrivenCommandsNeedApproval(t *testing.T) {
	rs := perm.DefaultRuleset()
	cases := [][]string{
		{"go", "build", "./..."},
		{"go", "build"},
		{"git", "status"},
		{"git", "diff"},
		{"git", "diff", "--stat", "HEAD"},
		{"git", "log", "--oneline", "-n", "5"},
		{"git", "show", "HEAD"},
	}
	for _, argv := range cases {
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("%v 应要求审批，实际 %q（%s）", argv, d.Verdict, d.Reason)
		}
	}
}

func TestMetacharForcesApprovalEvenWhenWhitelisted(t *testing.T) {
	rs := perm.DefaultRuleset()
	cases := [][]string{
		{"go", "test", "./...", "|", "tee"},
		{"go", "test", "$(rm -rf /)"},
		{"go", "test", "`id`"},
		{"cat", "a.txt", ">", "b.txt"},
		{"ls", "*.go"},
		{"go", "test", "x;y"},
		{"sh", "-c", "go test ./..."},
	}
	for _, argv := range cases {
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("%v 应要求审批，实际 %q（%s）", argv, d.Verdict, d.Reason)
		}
	}
}

// 元字符表逐字符过一遍：漏掉任何一个，模型都能用「首 token 命中白名单」
// 的名义拼出 shell 命令。
func TestEveryMetacharCharacterRequiresApproval(t *testing.T) {
	rs := perm.DefaultRuleset()
	for _, ch := range "|&;<>$`()*?[]~" {
		argv := []string{"go", "test", "./..." + string(ch) + "x"}
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("元字符 %q 应要求审批，实际 %q（%s）", ch, d.Verdict, d.Reason)
		}
	}
	for _, ch := range []string{"\n", "\r"} {
		d := rs.Evaluate(context.Background(), commandPrepared("go", "test", "./..."+ch))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("换行（%q）应要求审批，实际 %q", ch, d.Verdict)
		}
	}
}

func TestWhitelistRejectsUnsafeArguments(t *testing.T) {
	rs := perm.DefaultRuleset()
	cases := [][]string{
		// 读工作区外文件、读进程环境。
		{"cat", "/etc/passwd"},
		{"cat", "../secret"},
		{"head", "/Users/x/.ssh/id_rsa"},
		{"ls", "/"},
		// git 的读外、写外与变更操作。
		{"git", "diff", "--no-index", "/tmp/a", "/tmp/b"},
		{"git", "diff", "--output=/tmp/x"},
		{"git", "branch", "-D", "main"},
		// go 的任意命令执行与工作区外写盘。
		{"go", "test", "-exec=/tmp/evil", "./..."},
		{"go", "build", "-o", "/tmp/x", "./cmd"},
		{"go", "test", "-toolexec", "sh", "./..."},
	}
	for _, argv := range cases {
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("%v 应要求审批，实际 %q（%s）", argv, d.Verdict, d.Reason)
		}
	}
}

func TestLocalTightenAppliesToReadOnlyTool(t *testing.T) {
	dir := t.TempDir()
	body := `version: local-2
rules:
  - id: no-git-diff
    tool: git_diff
    programs: [git]
    reason: 本地要求手动确认 diff
`
	if err := os.WriteFile(filepath.Join(dir, "20-git.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	p := tools.Prepared{
		Call:    tools.Call{Name: "git_diff"},
		Spec:    tools.Spec{Name: "git_diff", Effects: []tools.Effect{tools.EffectRead}},
		Command: &tools.Command{Argv: []string{"git", "diff", "--no-color"}, Cwd: "/tmp"},
	}
	d := rs.Evaluate(context.Background(), p)
	if d.Verdict != perm.RequireUser {
		t.Fatalf("本地收紧规则必须能覆盖只读工具，得到 %q（%s）", d.Verdict, d.Reason)
	}
	if d.RuleID != "no-git-diff" || d.PolicyVersion == "" {
		t.Fatalf("收紧判定应带规则 id 与版本：%+v", d)
	}
}

func TestNonWhitelistedCommandRequiresApproval(t *testing.T) {
	rs := perm.DefaultRuleset()
	for _, argv := range [][]string{
		{"rm", "-rf", "build"},
		{"go", "generate"},
		{"git", "push"},
		{"python", "-c", "print(1)"},
		{"go"},
	} {
		d := rs.Evaluate(context.Background(), commandPrepared(argv...))
		if d.Verdict != perm.RequireUser {
			t.Fatalf("%v 应要求审批，实际 %q", argv, d.Verdict)
		}
	}
}

func TestReadOnlyToolsAreAllowed(t *testing.T) {
	rs := perm.DefaultRuleset()
	p := tools.Prepared{
		Call: tools.Call{Name: "read_output"},
		Spec: tools.Spec{Name: "read_output", Effects: []tools.Effect{tools.EffectRead}},
	}
	if d := rs.Evaluate(context.Background(), p); d.Verdict != perm.Allow {
		t.Fatalf("只读工具应放行，得到 %q（%s）", d.Verdict, d.Reason)
	}
	// 只读放行同样要带规则版本：git 工具的 exec.allow 审计依赖它。
	d := rs.Evaluate(context.Background(), p)
	if d.PolicyVersion == "" || d.RuleID != "read_only" {
		t.Fatalf("只读判定缺少规则版本/id：%+v", d)
	}
}

func TestHashIgnoresRuleOrder(t *testing.T) {
	a := perm.NewRuleset("v1", []perm.Rule{
		{ID: "b", Programs: []string{"b"}},
		{ID: "a", Programs: []string{"a"}},
	})
	b := perm.NewRuleset("v1", []perm.Rule{
		{ID: "a", Programs: []string{"a"}},
		{ID: "b", Programs: []string{"b"}},
	})
	if a.Hash != b.Hash {
		t.Fatalf("规则顺序不同时 hash 应相同：%s != %s", a.Hash, b.Hash)
	}
	c := perm.NewRuleset("v2", []perm.Rule{
		{ID: "a", Programs: []string{"a"}},
		{ID: "b", Programs: []string{"b"}},
	})
	if c.Hash == a.Hash {
		t.Fatal("版本变化应改变 hash")
	}
}

func TestLoaderLocalRulesCanOnlyTighten(t *testing.T) {
	dir := t.TempDir()
	body := `version: local-1
rules:
  - id: local-no-go-test
    programs: [go]
    subcommands: [test]
    reason: 本地策略禁止自动跑测试
`
	if err := os.WriteFile(filepath.Join(dir, "10-local.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := rs.Evaluate(context.Background(), commandPrepared("go", "test", "./..."))
	if d.Verdict != perm.RequireUser {
		t.Fatalf("本地收紧规则应要求审批，得到 %q", d.Verdict)
	}
	if d.RuleID != "local-no-go-test" {
		t.Fatalf("RuleID = %q", d.RuleID)
	}
	// 同文件里的放宽条目不存在：外部规则只能收紧，内置放行不受影响。
	if d := rs.Evaluate(context.Background(), commandPrepared("go", "vet", "./...")); d.Verdict != perm.Allow {
		t.Fatalf("未命中的规则不应影响其它命令，得到 %q", d.Verdict)
	}
}

func TestLoaderMissingDirIsFine(t *testing.T) {
	rs, err := perm.NewLoader(filepath.Join(t.TempDir(), "missing")).Load()
	if err != nil {
		t.Fatalf("目录不存在不应报错：%v", err)
	}
	if rs.Version == "" {
		t.Fatal("应加载内嵌规则")
	}
}

func TestLoaderRejectsBrokenRuleFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("rules: [{id: x}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := perm.NewLoader(dir).Load(); err == nil {
		t.Fatal("缺少 programs 的规则应让启动失败")
	}
}
