package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/config"
)

func writeConfig(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body = strings.ReplaceAll(body, "{{dir}}", dir)
	body = strings.ReplaceAll(body, "{{token}}", token)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, dir
}

const baseConfig = `# 顶层注释
server:
  listen: 127.0.0.1:8788
  token: file:{{token}}

storage:
  db_path: {{dir}}/data/jiaoag.db
  output_dir: {{dir}}/out

agent:
  # 单个任务的最大轮次
  max_rounds: 0

providers:
  - name: p
    type: openai
    base_url: https://example.com/v1
    model: m
    api_key: sk-test
`

func TestShowMarksSourcesAndHidesSecrets(t *testing.T) {
	p, _ := writeConfig(t, baseConfig)
	file, fields, err := config.Show(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(file, "config.yaml") {
		t.Fatalf("file = %q", file)
	}
	byPath := map[string]config.ShownField{}
	for _, f := range fields {
		byPath[f.Path] = f
	}
	listen := byPath["server.listen"]
	if listen.Value != "127.0.0.1:8788" || listen.Source != "config.yaml:3" {
		t.Fatalf("listen = %+v", listen)
	}
	token := byPath["server.token"]
	if !strings.Contains(token.Value, "file:") || !strings.Contains(token.Value, "字节") {
		t.Fatalf("token 应只报来源与长度，实际 %q", token.Value)
	}
	if strings.Contains(token.Value, "tok-123") {
		t.Fatal("token 明文泄漏")
	}
	key := byPath["providers[0].api_key"]
	if strings.Contains(key.Value, "sk-test") {
		t.Fatalf("api_key 明文泄漏：%q", key.Value)
	}
	if byPath["agent.approval_ttl"].Source != "默认值" {
		t.Fatalf("未写出的字段来源应为默认值：%+v", byPath["agent.approval_ttl"])
	}
}

func TestSetKeepsFormattingAndWritesBackup(t *testing.T) {
	p, _ := writeConfig(t, baseConfig)
	res, err := config.Set(config.SetOptions{Path: p, Key: "agent.max_rounds", Value: "5", Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Backup != "" {
		t.Fatalf("dry-run 不应写盘：%+v", res)
	}
	if n := strings.Count(res.Diff, "\n-"); n != 1 {
		t.Fatalf("diff 只应有一行删除，实际 %d：\n%s", n, res.Diff)
	}
	res, err = config.Set(config.SetOptions{Path: p, Key: "agent.max_rounds", Value: "5", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup == "" {
		t.Fatal("应生成备份")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"# 顶层注释", "# 单个任务的最大轮次", "max_rounds: 5", "\n\nproviders:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("写入后丢了 %q：\n%s", want, text)
		}
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Fatalf("备份不存在: %v", err)
	}
}

func TestSetInsertsMissingScalar(t *testing.T) {
	p, _ := writeConfig(t, baseConfig)
	if _, err := config.Set(config.SetOptions{Path: p, Key: "agent.approval_ttl", Value: "6h", Apply: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "  approval_ttl: 6h") {
		t.Fatalf("没有插入到 agent 块里：\n%s", raw)
	}
}

func TestSetRejectsUnknownPathAndBadValue(t *testing.T) {
	p, _ := writeConfig(t, baseConfig)
	_, err := config.Set(config.SetOptions{Path: p, Key: "agent.max_round", Value: "5"})
	if err == nil || !strings.Contains(err.Error(), "agent.max_rounds") {
		t.Fatalf("应提示最相近的路径，实际 %v", err)
	}
	_, err = config.Set(config.SetOptions{Path: p, Key: "agent.max_rounds", Value: "abc"})
	if err == nil || !strings.Contains(err.Error(), "整数") {
		t.Fatalf("应报告类型错误，实际 %v", err)
	}
	// 校验失败必须在落盘前拦住：-1 会被业务校验拒绝。
	_, err = config.Set(config.SetOptions{Path: p, Key: "agent.max_rounds", Value: "-1", Apply: true})
	if err == nil || !strings.Contains(err.Error(), "不能为负数") {
		t.Fatalf("应拒绝非法值，实际 %v", err)
	}
}

func TestCheckReportsUnreadableToken(t *testing.T) {
	body := strings.ReplaceAll(baseConfig, "file:{{token}}", "file:"+filepath.Join(t.TempDir(), "missing"))
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, "{{dir}}", t.TempDir())), 0o600); err != nil {
		t.Fatal(err)
	}
	_, issues, _, err := config.Check(p, "server")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range issues {
		if is.Level == "error" && strings.Contains(is.Message, "无法解析") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报告令牌无法解析：%+v", issues)
	}
}
