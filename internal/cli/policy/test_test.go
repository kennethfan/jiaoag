package policy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPolicyConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policy.d")
	if err := os.MkdirAll(policyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	body := "policy:\n  dir: " + policyDir + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunTestAllowsWhitelistedBuild(t *testing.T) {
	cfg := testPolicyConfig(t)
	var out, errOut bytes.Buffer
	code := runTest([]string{"-config", cfg, "--", "go", "build", "./..."}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "allow（放行）") || !strings.Contains(out.String(), "exec.go") {
		t.Fatalf("输出不对：%s", out.String())
	}
}

func TestRunTestFlagsRiskyAndShellForms(t *testing.T) {
	cfg := testPolicyConfig(t)
	var out, errOut bytes.Buffer
	if code := runTest([]string{"-config", cfg, "--", "npm", "publish"}, &out, &errOut); code != 1 {
		t.Fatalf("npm publish 应需要审批，exit=%d", code)
	}
	if !strings.Contains(out.String(), "risky_subcommand") {
		t.Fatalf("缺少风险原因：%s", out.String())
	}
	out.Reset()
	if code := runTest([]string{"-config", cfg, "--shell", "ls | wc -l"}, &out, &errOut); code != 1 {
		t.Fatalf("shell 形态应需要审批，exit=%d", code)
	}
	if !strings.Contains(out.String(), "shell:") {
		t.Fatalf("缺少 shell 原因：%s", out.String())
	}
	if strings.Contains(out.String(), "empty_argv") {
		t.Fatalf("shell 形态不该落到 empty_argv：%s", out.String())
	}
}

func TestRunTestUsesBuiltinWhenDirMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	missing := filepath.Join(dir, "nope", "policy.d")
	body := "policy:\n  dir: " + missing + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runTest([]string{"-config", cfg, "--", "go", "build", "./..."}, &out, &errOut); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "按内置模板") {
		t.Fatalf("应说明用的是内置模板：%s", out.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("policy test 不应创建规则目录")
	}
}
