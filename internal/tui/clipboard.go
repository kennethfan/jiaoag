package tui

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// clipboardMsg 是一次复制的结果。平台剪贴板写失败时 Update 会退回 OSC 52：
// 前者不依赖终端设置，后者在没有任何剪贴板工具的环境里是唯一办法。
type clipboardMsg struct {
	text  string
	lines int
	err   error
}

// writeClipboard 是平台剪贴板写入，抽成变量以便测试替换。
var writeClipboard = platformClipboard

// copyCmd 把文本写进剪贴板。子进程耗时在毫秒级，直接同步执行即可；
// 真正慢的环境会走 OSC 52 兜底，不阻塞界面循环。
func copyCmd(text string, lines int) tea.Cmd {
	return func() tea.Msg {
		return clipboardMsg{text: text, lines: lines, err: writeClipboard(text)}
	}
}

// platformClipboard 依次尝试平台自带的剪贴板命令。
//
// macOS 一定有 pbcopy；Windows 有 clip；Linux 桌面环境按 Wayland → X11
// 的顺序找。全都不可用时返回错误，由调用方退回 OSC 52。
func platformClipboard(text string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	case "windows":
		candidates = [][]string{{"clip"}}
	default:
		candidates = [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}

	var lastErr error
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate[0])
		if err != nil {
			lastErr = err
			continue
		}
		cmd := exec.Command(path, candidate[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的剪贴板命令")
	}
	return lastErr
}
