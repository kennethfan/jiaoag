package tools

import (
	"strings"
	"testing"
)

// 白名单按大写比较：Windows 上父进程给的是 "Path"，按原样查表会把它丢掉，
// 子进程就连命令都找不到。
func TestEnvAllowedIsCaseInsensitive(t *testing.T) {
	for _, key := range []string{"PATH", "Path", "path", "SystemRoot", "SYSTEMROOT", "ComSpec", "PSModulePath", "USERPROFILE", "APPDATA"} {
		if !envAllowed(key) {
			t.Fatalf("%q 应被放行", key)
		}
	}
}

// 密钥类变量怎么拼都不许过：这是白名单里唯一真正的安全约束。
func TestEnvAllowedRejectsSecrets(t *testing.T) {
	for _, key := range []string{
		"OPENAI_API_KEY", "openai_api_key", "GITHUB_TOKEN", "github_token",
		"AWS_SECRET_ACCESS_KEY", "DB_PASSWORD", "db_password", "MYSQL_PASSWD",
		"GOOGLE_APPLICATION_CREDENTIALS", "my_key", "SESSION_TOKEN",
	} {
		if envAllowed(key) {
			t.Fatalf("%q 不该被放行", key)
		}
	}
}

func TestCommandEnvFiltersAndKeepsOrder(t *testing.T) {
	in := []string{
		"Path=/usr/bin",
		"HOME=/root",
		"OPENAI_API_KEY=sk-secret",
		"LC_ALL=zh_CN.UTF-8",
		"UNRELATED=1",
		"TMPDIR=/tmp",
	}
	got := commandEnv(in)
	want := []string{"Path=/usr/bin", "HOME=/root", "LC_ALL=zh_CN.UTF-8", "TMPDIR=/tmp"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commandEnv = %v，期望 %v", got, want)
	}
}

// 没有 "=" 的畸形条目要丢掉，不能原样传给子进程。
func TestCommandEnvSkipsMalformedEntries(t *testing.T) {
	got := commandEnv([]string{"PATH=/bin", "garbage", "=value"})
	if strings.Join(got, "|") != "PATH=/bin" {
		t.Fatalf("commandEnv = %v", got)
	}
}
