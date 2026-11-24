//go:build windows

package tools

// shellArgv 在 Windows 上用 cmd /c。它支持管道与重定向，但引号规则与 sh
// 不同——审批面板里展示的就是这条 cmd 命令，不会再被改写。
func shellArgv(text string) []string { return []string{"cmd", "/c", text} }
