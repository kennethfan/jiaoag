// Package memory 管理跨会话的项目记忆文件。
//
// 记忆是工作区根目录下的 MEMORY.md：内容会作为一层提示词注入每个任务，
// 因此它天然按工作区隔离，也能被用户直接查看、编辑、纳入版本控制。
// 它只回答「这个项目长期成立的事实」，不承担任务进度或会话历史。
package memory

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// FileName 是记忆文件在工作区根目录下的名字。
	//
	// 放在根目录而不是 .jiaoag/ 子目录：落盘走 workspace.Write，它要求父目录
	// 已存在；为一个文件隐式建目录会让「路径写错」变成「在奇怪的地方建目录」。
	FileName = "MEMORY.md"

	// MaxLoadBytes 是注入提示词的记忆上限。记忆是每轮都要付的上下文成本，
	// 不该无限增长；超过时截断并提示模型精简。
	MaxLoadBytes = 8 << 10

	// MaxEntryRunes 是单条记忆的长度上限。
	MaxEntryRunes = 500
)

// Path 返回工作区下的记忆文件绝对路径。
func Path(root string) string { return filepath.Join(root, FileName) }

// Load 读取记忆文件。
//
// 文件不存在返回空串与 nil：没有记忆是正常状态，不是错误。超过 MaxLoadBytes
// 时只返回前 MaxLoadBytes 字节（按 rune 边界回退），truncated 为真，并在正文
// 末尾附一句提示。
func Load(root string) (text string, truncated bool, err error) {
	f, err := os.Open(Path(root))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("memory: 打开 %s: %w", FileName, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, MaxLoadBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("memory: 读取 %s: %w", FileName, err)
	}
	truncated = len(data) > MaxLoadBytes
	if truncated {
		data = data[:MaxLoadBytes]
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
	}
	text = strings.TrimSpace(string(data))
	if truncated {
		text += fmt.Sprintf("\n\n（%s 超过 %d 字节，只加载了前面部分；请精简旧条目）", FileName, MaxLoadBytes)
	}
	return text, truncated, nil
}

// Section 把记忆正文包成提示词层。没有记忆时返回空串，调用方据此决定是否
// 追加该层。
func Section(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "## 跨会话项目记忆（" + FileName + "）\n" +
		"以下是此前会话或用户维护的项目事实，供参考；与当前任务指令冲突时，以当前指令为准。\n" +
		"需要长期保留的新事实，可用 remember 工具追加（会先显示 diff 等待批准）。" +
		"不要记录密钥、口令或个人隐私。\n\n" + text
}

// FormatEntry 把一条事实格式化成记忆文件里的一行。
//
// 日期只用于让用户判断新旧；同一天写入多条也能按行区分。换行会被压成空格，
// 避免一条记忆把整个文件的条目结构打乱。
func FormatEntry(text string, at time.Time) string {
	text = strings.Join(strings.Fields(text), " ")
	return fmt.Sprintf("- %s: %s", at.Format("2006-01-02"), text)
}
