//go:build !windows

package tools

// shellArgv 把一条 shell 命令包成宿主平台上的执行形态。
//
// 这里固定用 sh（POSIX）而不是用户的 $SHELL：审批面板展示的是这条命令
// 本身，执行器必须与审批时看到的语义一致，不能随用户环境变化。
func shellArgv(text string) []string { return []string{"sh", "-c", text} }
