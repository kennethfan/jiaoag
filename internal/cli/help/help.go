// Package help 统一各子命令的帮助文本。
//
// flag 包自带的 -h 只会打「Usage of jiaoag xxx:」加一串没有上下文的默认值，
// 看不出这个子命令是干什么的、还有哪些子命令、参数长什么样。每个子命令都用
// 同一套结构输出：一句话说明 → 用法 → 子命令 → 标志 → 示例。
package help

import (
	"bytes"
	"flag"
	"fmt"
	"io"
)

// Command 是一条子命令：名字与一句话说明。
type Command struct {
	Name string
	Desc string
}

// Spec 是一份帮助文本的全部素材。Flags 为 nil 时不输出标志段。
type Spec struct {
	Summary  string
	Usage    []string
	Commands []Command
	Flags    *flag.FlagSet
	Examples []string
}

// Write 按固定顺序输出帮助。标志段直接复用 flag 的 PrintDefaults，
// 这样默认值与说明只维护一份。
func Write(w io.Writer, s Spec) {
	if s.Summary != "" {
		fmt.Fprintf(w, "%s\n\n", s.Summary)
	}
	if len(s.Usage) > 0 {
		fmt.Fprintln(w, "用法：")
		for _, line := range s.Usage {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	if len(s.Commands) > 0 {
		fmt.Fprintln(w, "\n子命令：")
		width := 0
		for _, c := range s.Commands {
			if len(c.Name) > width {
				width = len(c.Name)
			}
		}
		for _, c := range s.Commands {
			fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Desc)
		}
	}
	if s.Flags != nil {
		orig := s.Flags.Output()
		var buf bytes.Buffer
		s.Flags.SetOutput(&buf)
		s.Flags.PrintDefaults()
		s.Flags.SetOutput(orig)
		// 没有声明任何标志的包（如 plugin-example）不输出空标题。
		if buf.Len() > 0 {
			fmt.Fprintln(w, "\n标志：")
			// PrintDefaults 已经按 "  -name value\n    \t说明" 缩进，原样搬过去。
			_, _ = w.Write(buf.Bytes())
		}
	}
	if len(s.Examples) > 0 {
		fmt.Fprintln(w, "\n示例：")
		for _, e := range s.Examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
}
