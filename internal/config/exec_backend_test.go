package config

import (
	"strings"
	"testing"
)

func TestExecBackendWhitelist(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		want    string
	}{
		{"direct 通过", DefaultExecBackend, ""},
		{"sandbox 通过", SandboxExecBackend, ""},
		{"未知后端", "docker", "exec.backend"},
		{"拼写错误", "SandBox", "exec.backend"},
	}
	for _, c := range cases {
		cfg := validConfig()
		cfg.Exec.Backend = c.backend
		err := cfg.ValidateClient()
		switch {
		case c.want == "" && err != nil:
			t.Fatalf("%s：不该报错，得到 %v", c.name, err)
		case c.want != "" && err == nil:
			t.Fatalf("%s：应当报错", c.name)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Fatalf("%s：错误信息应提到 %q，得到 %v", c.name, c.want, err)
		}
	}
}
