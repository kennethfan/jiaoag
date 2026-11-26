package config

import (
	"os"
	"strings"
	"testing"
)

func TestRunPathAndShowJSON(t *testing.T) {
	var out, errOut strings.Builder
	if code := Run([]string{"path", "-config", "/tmp/nope.yaml"}, &out, &errOut, strings.NewReader("")); code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "/tmp/nope.yaml") {
		t.Fatalf("out=%s", out.String())
	}
}

func TestRunSetRejectsSecretArgument(t *testing.T) {
	cfg := writeCLIConfig(t, "server:\n  token: file:/tmp/x\nproviders:\n  - name: p\n    type: openai\n    base_url: https://example.com/v1\n    model: m\n    api_key: sk-old\n")
	var out, errOut strings.Builder
	code := Run([]string{"set", "providers[0].api_key", "sk-new", "-config", cfg}, &out, &errOut, strings.NewReader(""))
	if code != 2 || !strings.Contains(errOut.String(), "敏感项") {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if strings.Contains(out.String(), "sk-new") {
		t.Fatal("拒绝时不该输出值")
	}
}

func TestRunSetReadsSecretFromStdin(t *testing.T) {
	cfg := writeCLIConfig(t, "providers:\n  - name: p\n    type: openai\n    base_url: https://example.com/v1\n    model: m\n    api_key: sk-old\n")
	var out, errOut strings.Builder
	code := Run([]string{"set", "providers[0].api_key", "-", "-config", cfg, "-y"}, &out, &errOut, strings.NewReader("sk-new\n"))
	if code != 0 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	body := readFile(t, cfg)
	if !strings.Contains(body, "api_key: sk-new") || strings.Contains(body, "sk-old") {
		t.Fatalf("文件未更新：\n%s", body)
	}
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeCLIConfig(t *testing.T, body string) string {
	t.Helper()
	p := t.TempDir() + "/config.yaml"
	if err := writeFile(p, body); err != nil {
		t.Fatal(err)
	}
	return p
}
