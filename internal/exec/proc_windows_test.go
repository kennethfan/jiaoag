//go:build windows

package exec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procGroup 必须收掉整棵进程树：只杀父进程会留下它派生的子进程，
// 这些残留会继续占端口与 CPU，而服务端以为命令已经结束。
func TestKillProcessGroupKillsChildTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// PowerShell 起一个后台子进程，把它的 pid 写进文件，然后自己等它结束。
	// 这样 kill 面对的是一个「父进程挂着、子进程也在跑」的树。
	script := fmt.Sprintf(
		`$p = Start-Process -FilePath ping -ArgumentList '-n','30','127.0.0.1' -PassThru -WindowStyle Hidden;`+
			`$p.Id | Out-File -Encoding ascii -NoNewline '%s';`+
			`Wait-Process -Id $p.Id`,
		pidFile,
	)

	pg, err := newProcGroup(Limits{})
	if err != nil {
		t.Fatalf("newProcGroup: %v", err)
	}
	defer pg.close()

	cmd := exec.Command("powershell", "-NoProfile", "-Command", script)
	pg.beforeStart(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动 powershell：%v", err)
	}
	if err := pg.afterStart(cmd); err != nil {
		pg.kill(cmd)
		_ = cmd.Wait()
		t.Skipf("无法把进程赋进 Job Object，跳过：%v", err)
	}

	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		pg.kill(cmd)
		_ = cmd.Wait()
		t.Skip("没有读到子进程 pid，跳过（powershell 可能被策略限制）")
	}

	pg.kill(cmd)
	_ = cmd.Wait()

	// 子进程应当随进程树一起消失；给它最多 5 秒的收尸时间。
	gone := time.Now().Add(5 * time.Second)
	for time.Now().Before(gone) {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", childPID), "/NH").Output()
		if err == nil && !strings.Contains(strings.ToLower(string(out)), "ping.exe") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("kill 之后子进程 %d 仍然存活（进程树没收干净）", childPID)
}
