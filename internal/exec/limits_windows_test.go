//go:build windows && (amd64 || arm64)

package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// queryLimits 读回 Job Object 上实际生效的限额，失败信息里带上它，
// 才能分清是「没设上」还是「设上了但没生效」。
func queryLimits(g *procGroup) string {
	if g.job == 0 {
		return "没有 Job Object（已退化为 taskkill）"
	}
	var info jobObjectExtendedLimitInfo
	r, _, callErr := procQueryInformationJobObject.Call(
		uintptr(g.job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		0,
	)
	if r == 0 {
		return fmt.Sprintf("查询 Job 限制失败: %v", callErr)
	}
	return fmt.Sprintf("LimitFlags=0x%x ProcessMemoryLimit=%d PerProcessUserTimeLimit=%d",
		info.BasicLimitInformation.LimitFlags,
		info.ProcessMemoryLimit,
		info.BasicLimitInformation.PerProcessUserTimeLimit,
	)
}

// Job Object 的内存上限必须真的生效：分配远超上限的缓冲区应当失败。
func TestJobObjectMemoryLimit(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("没有 powershell，跳过 Job Object 内存上限用例")
	}

	// 600MB 足以超过 256MB 的单进程上限，又远小于 CI runner 的物理内存，
	// 因此「无限制时必须成功」这条对照也成立。
	const script = `$a = New-Object byte[] (600MB); "allocated=$($a.Length)"`

	run := func(l Limits) (int, string, string) {
		g, err := newProcGroup(l)
		if err != nil {
			t.Fatalf("newProcGroup(%+v): %v", l, err)
		}
		defer g.close()

		var out bytes.Buffer
		cmd := exec.Command(ps, "-NoProfile", "-Command", script)
		cmd.Stdout = &out
		cmd.Stderr = &out
		g.beforeStart(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 powershell: %v", err)
		}
		if err := g.afterStart(cmd); err != nil {
			g.kill(cmd)
			_ = cmd.Wait()
			t.Fatalf("把子进程加入 Job Object: %v", err)
		}
		limits := queryLimits(g)
		werr := cmd.Wait()
		if werr == nil {
			return 0, out.String(), limits
		}
		var exit *exec.ExitError
		if !errors.As(werr, &exit) {
			t.Fatalf("等待 powershell: %v（输出 %q）", werr, out.String())
		}
		return exit.ExitCode(), out.String(), limits
	}

	// 对照：没有限制时同一段脚本必须成功，否则后面的断言没有意义。
	if code, out, _ := run(Limits{}); code != 0 || !strings.Contains(out, "allocated=") {
		t.Skipf("无限制时分配 600MB 没有成功（退出码 %d，输出 %q），环境内存不足或 powershell 行为异常，跳过", code, out)
	}

	code, out, limits := run(Limits{MemoryMB: 256})
	if code == 0 {
		t.Fatalf("限制 256MB 后分配 600MB 仍然成功（输出 %q，%s）", out, limits)
	}
}

// 通过 Direct.Run 走完整接线：Prepare 不应再把 Windows 上的内存/CPU 报成降级，
// 带限制的普通命令要能正常跑完。
func TestWindowsLimitsNotDegraded(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("没有 powershell，跳过完整接线用例")
	}

	b := NewDirect()
	cmd := Command{
		Argv:    []string{ps, "-NoProfile", "-Command", `Write-Output ok`},
		Cwd:     t.TempDir(),
		Timeout: 30 * time.Second,
		Env:     []string{},
		Limits:  Limits{CPUSeconds: 5, MemoryMB: 128},
	}
	plan, err := b.Prepare(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if plan.LimitsDegraded() {
		t.Fatalf("Windows 上 CPU/内存不应降级：%v", strings.Join(plan.LimitSupport.Missing(plan.Limits), ","))
	}

	res, err := b.Run(context.Background(), plan, DefaultSpec(""))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("有限制时普通命令应当正常跑完：code=%d output=%q", res.ExitCode, res.Output)
	}
}
