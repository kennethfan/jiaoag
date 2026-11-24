// 本文件是 TUI 的样式层：语义色、宽度与截断。
//
// 刻意不引入 lipgloss：整个界面只用 SGR 序列就能表达，多一个依赖换来的只是
// 更漂亮的 API，而宽度计算（东亚字符占两列）本来也要自己写。样式只在渲染时
// 施加，m.dialog 里存的始终是纯文本——测试与日志不该依赖转义序列。
package tui

import (
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// style 是语义色。界面上每个颜色都对应一种含义，不做装饰性用色：
// 同一个动作在对话区、面板、状态栏里永远是同一个颜色。
type style int

const (
	stylePlain style = iota
	styleStrong
	styleAccent
	styleOK
	styleWarn
	styleErr
	styleMuted
	styleAdd
	styleDel
)

// sgr 是样式到 ANSI 序列的映射。用 8 色基色而不是 256 色：终端主题各异，
// 基色在任何配色下都能保持对比度；需要更细的层次时用加粗而不是换色。
var sgr = map[style]string{
	styleStrong: "\x1b[1m",
	styleAccent: "\x1b[36m",
	styleOK:     "\x1b[32m",
	styleWarn:   "\x1b[33m",
	styleErr:    "\x1b[31m",
	styleMuted:  "\x1b[90m",
	styleAdd:    "\x1b[32m",
	styleDel:    "\x1b[31m",
}

const sgrReset = "\x1b[0m"

// styler 决定是否真的产生转义序列。关闭时所有 paint 都是恒等变换，
// 布局与文本内容完全不变——这是 NO_COLOR 能安全降级的前提。
type styler struct{ enabled bool }

// newStyler 读取 NO_COLOR 与 TERM。按 no-color.org 的约定，只要变量存在
// （哪怕为空串以外的任何值）就关闭颜色；TERM=dumb 同样关闭。
func newStyler() styler {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return styler{}
	}
	if os.Getenv("TERM") == "dumb" {
		return styler{}
	}
	return styler{enabled: true}
}

func (s styler) paint(kind style, text string) string {
	if !s.enabled || kind == stylePlain || text == "" {
		return text
	}
	seq, ok := sgr[kind]
	if !ok {
		return text
	}
	return seq + text + sgrReset
}

// stylizeLine 按行首标记给对话行上色。
//
// 用前缀判定而不是在写入时打标：历史行已经在 m.dialog 里，按语义前缀着色
// 不需要改动事件的写入路径，也保证回放出来的历史与新行样式一致。
func (s styler) stylizeLine(line string) string {
	if !s.enabled || line == "" {
		return line
	}
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	switch {
	case strings.HasPrefix(trimmed, "› "):
		return indent + s.paint(styleAccent, "› ") + s.paint(styleStrong, trimmed[2:])
	case strings.HasPrefix(trimmed, "jiaoag: "):
		return indent + s.paint(styleStrong, "jiaoag") + s.paint(styleMuted, ": ") + trimmed[len("jiaoag: "):]
	case strings.HasPrefix(trimmed, "你 "):
		return indent + s.paint(styleAccent, "你 ") + trimmed[len("你 "):]
	case strings.HasPrefix(trimmed, "⚙ "), strings.HasPrefix(trimmed, "⚙"):
		return indent + s.paint(styleMuted, trimmed)
	case strings.HasPrefix(trimmed, "↳ "):
		return indent + s.paint(styleMuted, trimmed)
	case strings.HasPrefix(trimmed, "✔ "):
		return indent + s.paint(styleOK, trimmed)
	case strings.HasPrefix(trimmed, "✖ "), strings.HasPrefix(trimmed, "失败："):
		return indent + s.paint(styleErr, trimmed)
	case strings.HasPrefix(trimmed, "⚠ "), strings.HasPrefix(trimmed, "⋯ "):
		return indent + s.paint(styleWarn, trimmed)
	case strings.HasPrefix(trimmed, "— 审批"):
		return indent + s.paint(styleMuted, trimmed)
	case strings.HasPrefix(trimmed, "错误 "):
		return indent + s.paint(styleErr, trimmed)
	case strings.HasPrefix(trimmed, "!"):
		// 本机命令：回显与输出都用 muted，和人机对话区分开。
		return indent + s.paint(styleMuted, trimmed)
	}
	return line
}

// stylizeDiffLine 给 diff 行着色。增删整行上色，比只染首字符更容易扫。
func (s styler) stylizeDiffLine(line string) string {
	if !s.enabled || line == "" {
		return line
	}
	switch {
	case strings.HasPrefix(line, "@@"):
		return s.paint(styleAccent, line)
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return s.paint(styleMuted, line)
	case strings.HasPrefix(line, "+"):
		return s.paint(styleAdd, line)
	case strings.HasPrefix(line, "-"):
		return s.paint(styleDel, line)
	}
	return line
}

// displayWidth 返回字符串在终端里占的列数：CJK 与全角字符占两列，
// 组合符与零宽字符不占位。ANSI 序列不计入宽度。
func displayWidth(s string) int {
	width := 0
	for _, r := range stripANSI(s) {
		width += runeWidth(r)
	}
	return width
}

// runeWidth 是 displayWidth 的单字符版本。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), r == '\u200b':
		return 0
	case isWide(r):
		return 2
	default:
		return 1
	}
}

// isWide 覆盖东亚宽字符的主要区段。终端实际宽度还受 locale 影响，
// 但界面只需要一个稳定的估算：宁可多算一列，也不要把边框撑破。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0x303E, // CJK 部首、标点
		r >= 0x3041 && r <= 0x33FF, // 假名、CJK 兼容
		r >= 0x3400 && r <= 0x4DBF, // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF, // CJK 基本区
		r >= 0xA000 && r <= 0xA4CF, // 彝文
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B 及以后
		return true
	}
	return false
}

// truncateDisplay 按显示宽度截断并补省略号。宽度不够放下省略号时直接硬截，
// 不留下一个孤零零的「…」。
func truncateDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(s) <= width {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runeWidth(r)
		if used+w > width-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteString("…")
	return b.String()
}

// padDisplay 在右侧补空格到指定显示宽度；已经超宽时原样返回。
func padDisplay(s string, width int) string {
	if w := displayWidth(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// stripANSI 去掉 SGR 序列。只需要处理 CSI 的 m 结尾形式——界面不产生其他
// 控制序列，做一个通用状态机反而是多余的攻击面。
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	skip := false
	for i := 0; i < len(s); i++ {
		if skip {
			if s[i] == 'm' {
				skip = false
			}
			continue
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			skip = true
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// wrapDisplay 按显示宽度折行。wrapText 按 rune 数切，遇到中文会超宽——
// 面板的宽度约束是列数，不是字符数。
// wrapStyled 按显示宽度折行，但保留 ANSI 转义序列（不把它拆到两行里，
// 也不把转义字节算进宽度）。上面 wrapDisplay 只适合纯文本。
func wrapStyled(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var (
		out  []string
		line strings.Builder
		used int
	)
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++
			}
			line.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := runeWidth(r)
		if used+w > width && used > 0 {
			out = append(out, line.String())
			line.Reset()
			used = 0
		}
		line.WriteString(s[i : i+size])
		used += w
		i += size
	}
	out = append(out, line.String())
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func wrapDisplay(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	var (
		out  []string
		line strings.Builder
		used int
	)
	for _, r := range s {
		w := runeWidth(r)
		if used+w > width && used > 0 {
			out = append(out, line.String())
			line.Reset()
			used = 0
		}
		line.WriteRune(r)
		used += w
	}
	if line.Len() > 0 {
		out = append(out, line.String())
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}
