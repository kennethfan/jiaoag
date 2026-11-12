// Package patch 生成变更提案所需的摘要与 unified diff。
//
// 它只做纯计算：不读盘、不写盘、不认识工作区。写入的安全边界在
// internal/workspace，审批流程在 internal/agent。
package patch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/kennethfan/jiaoag/internal/limits"
)

// MaxDiffBytes 是单条 diff 的硬上限。
//
// 与 tools.MaxOutputBytes 同源（limits.MaxPayloadBytes）：一条提案的
// 事件里除了 diff 还有别的字段，两个上限都在 256 KB，才能保证一个事件
// 不会把日志和界面撑爆。
const MaxDiffBytes = limits.MaxPayloadBytes

// Hash 返回内容的 sha256 十六进制摘要。空内容也有确定的摘要（不是空串）。
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ShortHash 把摘要截短到前 12 位，仅供界面与错误信息展示。
func ShortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// Endings 是文本的换行风格。
type Endings string

const (
	// LF 是 Unix 换行。
	LF Endings = "\n"
	// CRLF 是 Windows 换行。
	CRLF Endings = "\r\n"
)

// LineEndings 探测文本的换行风格：出现一处 CRLF 就按 CRLF 处理。
//
// 只影响 diff 的呈现与写入时的还原，不改变内容本身。
func LineEndings(b []byte) Endings {
	if bytes.Contains(b, []byte("\r\n")) {
		return CRLF
	}
	return LF
}

// ToLF 把 CRLF 归一成 LF。
//
// 归一化只用于匹配与 diff：模型给的是 LF，磁盘上的文件可能是 CRLF，
// 直接比较会让「只改一行」变成「整篇重写」。
func ToLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// Normalize 把文本统一成 e 指定的换行风格。
func Normalize(b []byte, e Endings) []byte {
	lf := ToLF(b)
	if e != CRLF {
		return lf
	}
	return bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
}

// Unified 生成标准 unified diff（3 行上下文）。
//
// exists 为 false 表示新建文件：此时 old 应为空，diff 头用 /dev/null，
// 不写 a/<path>。超过 MaxDiffBytes 时按 UTF-8 字符边界截断并返回
// truncated=true——宁可让用户看到「这里被截了」，也不要吞掉半条记录。
func Unified(path string, old, new []byte, exists bool) (string, bool, error) {
	fromFile := "a/" + path
	if !exists {
		fromFile = "/dev/null"
	}
	ud := difflib.UnifiedDiff{
		FromFile: fromFile,
		ToFile:   "b/" + path,
		Context:  3,
	}
	// 新建文件不能把空内容塞进 A：SplitLines("") 会产出一个空行，
	// diff 里就多出一条莫名其妙的删除行。
	if exists {
		ud.A = splitLines(old)
	}
	ud.B = splitLines(new)

	text, err := difflib.GetUnifiedDiffString(ud)
	if err != nil {
		return "", false, fmt.Errorf("patch: 生成 %s 的 diff: %w", path, err)
	}
	if len(text) <= MaxDiffBytes {
		return text, false, nil
	}
	cut := MaxDiffBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[已截断：完整 diff %d 字节，只显示前 %d 字节。文件太大，建议拆分改动]\n",
		len(text), cut), true, nil
}

// splitLines 把字节切成 difflib 的行序列，每行都以 "\n" 结尾。
//
// 不用 difflib.SplitLines：它对以换行结尾的文本会多造一个空行
// （"x\n" → ["x\n", "\n"]），hunk 头和行数因此整体多 1。空内容对应空序列，
// 而不是「一个空行」。
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.SplitAfter(string(ToLF(b)), "\n")
	last := len(lines) - 1
	if lines[last] == "" {
		// 文本以换行结尾：SplitAfter 会留下一个空尾元素，丢掉它。
		return lines[:last]
	}
	// 文本没有结尾换行：补一个，否则 diff 输出会把相邻两行粘成一行。
	lines[last] += "\n"
	return lines
}

// Stats 统计 diff 里新增与删除的行数，不含 ---/+++ 文件头。
//
// 界面用它在提案行上显示「+12 -3」，让用户不展开也知道改动规模。
func Stats(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}
