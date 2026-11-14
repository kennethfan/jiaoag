package config

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Server:  Server{Listen: "127.0.0.1:8787", Token: "tok"},
		Storage: Storage{DBPath: "/tmp/x.db", OutputDir: "/tmp/out"},
		Exec: Exec{
			Backend:        DefaultExecBackend,
			TimeoutSeconds: 120,
			EventLimit:     8192,
			LogLimit:       4 << 20,
		},
	}
}

func TestExecLimitsValidation(t *testing.T) {
	cases := []struct {
		name string
		l    ExecLimits
		want string
	}{
		{"零值通过", ExecLimits{}, ""},
		{"三项合法通过", ExecLimits{CPUSeconds: 300, MemoryMB: 4096, MaxFiles: 1024}, ""},
		{"CPU 为负", ExecLimits{CPUSeconds: -1}, "cpu_seconds"},
		{"内存为负", ExecLimits{MemoryMB: -1}, "memory_mb"},
		{"句柄为负", ExecLimits{MaxFiles: -1}, "max_files"},
		{"内存太小", ExecLimits{MemoryMB: 32}, "memory_mb"},
	}
	for _, c := range cases {
		cfg := validConfig()
		cfg.Exec.Limits = c.l
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
