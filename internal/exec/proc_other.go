//go:build !unix && !windows

package exec

import "os/exec"

// procGroup 在既不是 Unix 也不是 Windows 的平台上退化为「只杀父进程」。
type procGroup struct{}

func newProcGroup(Limits) (*procGroup, error) { return &procGroup{}, nil }

func (g *procGroup) beforeStart(*exec.Cmd) {}

func (g *procGroup) afterStart(*exec.Cmd) error { return nil }

func (g *procGroup) kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (g *procGroup) close() {}
