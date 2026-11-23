//go:build darwin

package exec

import (
	"strings"
	"testing"
)

func TestSandboxProfileAllowsWorkspaceAndDeniesSecrets(t *testing.T) {
	profile := sandboxProfile(Command{
		Cwd: "/tmp/ws",
		Env: []string{"HOME=/home/u", "TMPDIR=/tmp"},
	})
	for _, want := range []string{
		`(allow file-write*`,
		`(subpath "/tmp/ws")`,
		`(deny file-read*`,
		`(subpath "/home/u/.ssh")`,
		`(subpath "/home/u/.aws")`,
		`(subpath "/home/u/.config")`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile 缺少 %q:\n%s", want, profile)
		}
	}
}

// 工作区落在敏感目录下时，宁可放弃该条 deny，也不能把工作区本身封死。
func TestSandboxProfileSkipsDenyContainingWorkspace(t *testing.T) {
	profile := sandboxProfile(Command{
		Cwd: "/home/u/.config/project",
		Env: []string{"HOME=/home/u"},
	})
	if strings.Contains(profile, `"/home/u/.config"`) {
		t.Fatalf("工作区在 .config 下时不应 deny 该目录:\n%s", profile)
	}
}
