package patch

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Hunk 是 unified diff 里一个 @@ 段。Lines 保留行首的 ' ' / '-' / '+' 前缀，
// 应用时据此判断这一行是上下文、删除还是新增。
type Hunk struct {
	Index    int
	Header   string
	Lines    []string
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Added    int
	Removed  int
}

// truncatedMarker 是 Unified 在 diff 超过上限时追加的说明行。出现它说明
// diff 不完整，任何按块应用都不可靠。
const truncatedMarker = "[已截断"

// ParseHunks 解析 unified diff 的各个块。diff 里的文件头（--- / +++）会被跳过。
func ParseHunks(diff string) ([]Hunk, error) {
	text := string(ToLF([]byte(diff)))
	if strings.Contains(text, truncatedMarker) {
		return nil, fmt.Errorf("patch: diff 已被截断，无法按块应用")
	}
	// 保留行尾换行：新增行要原样拼回内容。
	lines := strings.SplitAfter(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	var (
		hunks []Hunk
		cur   *Hunk
	)
	flush := func() error {
		if cur == nil {
			return nil
		}
		if err := cur.validate(); err != nil {
			return err
		}
		cur.Index = len(hunks)
		hunks = append(hunks, *cur)
		cur = nil
		return nil
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "@@ ") {
			if err := flush(); err != nil {
				return nil, err
			}
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			cur = &h
			continue
		}
		if cur == nil {
			continue // 文件头或块外内容
		}
		if line == "" {
			// 末尾换行被 TrimRight 掉之后可能留一个空串；块内空行应以 " " 出现。
			continue
		}
		cur.Lines = append(cur.Lines, line)
		switch line[0] {
		case '+':
			cur.Added++
		case '-':
			cur.Removed++
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(hunks) == 0 {
		return nil, fmt.Errorf("patch: diff 里没有可解析的块")
	}
	return hunks, nil
}

func parseHunkHeader(line string) (Hunk, error) {
	// 形如 @@ -1,3 +1,4 @@ 或 @@ -1 +1 @@
	body := strings.TrimPrefix(line, "@@ ")
	if i := strings.Index(body, " @@"); i >= 0 {
		body = body[:i]
	}
	fields := strings.Fields(body)
	if len(fields) != 2 {
		return Hunk{}, fmt.Errorf("patch: 无法解析块头 %q", line)
	}
	oldStart, oldCount, err := parseRange(fields[0], '-')
	if err != nil {
		return Hunk{}, fmt.Errorf("patch: 块头 %q: %w", line, err)
	}
	newStart, newCount, err := parseRange(fields[1], '+')
	if err != nil {
		return Hunk{}, fmt.Errorf("patch: 块头 %q: %w", line, err)
	}
	return Hunk{
		Header:   line,
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
	}, nil
}

func parseRange(field string, sign byte) (start, count int, err error) {
	if len(field) < 2 || field[0] != sign {
		return 0, 0, fmt.Errorf("范围 %q 缺少前缀 %q", field, string(sign))
	}
	body := field[1:]
	count = 1
	if i := strings.IndexByte(body, ','); i >= 0 {
		count, err = strconv.Atoi(body[i+1:])
		if err != nil {
			return 0, 0, fmt.Errorf("范围 %q 的行数不是整数", field)
		}
		body = body[:i]
	}
	start, err = strconv.Atoi(body)
	if err != nil {
		return 0, 0, fmt.Errorf("范围 %q 的起始行不是整数", field)
	}
	if start < 0 || count < 0 {
		return 0, 0, fmt.Errorf("范围 %q 含负数", field)
	}
	return start, count, nil
}

func (h *Hunk) validate() error {
	var oldLines, newLines int
	for _, line := range h.Lines {
		switch line[0] {
		case ' ':
			// 上下文行同时存在于新旧两侧。
			oldLines++
			newLines++
		case '-':
			oldLines++
		case '+':
			newLines++
		case '\\':
			// "\ No newline at end of file" 这类标记不计入行数
		default:
			return fmt.Errorf("patch: 块 %q 里有无法识别的行 %q", h.Header, line)
		}
	}
	if oldLines != h.OldCount {
		return fmt.Errorf("patch: 块 %q 声明旧侧 %d 行，实际 %d 行", h.Header, h.OldCount, oldLines)
	}
	if newLines != h.NewCount {
		return fmt.Errorf("patch: 块 %q 声明新侧 %d 行，实际 %d 行", h.Header, h.NewCount, newLines)
	}
	return nil
}

// Selection 校验并归一化选中的块下标：去重、升序；空表示「全部」。
// 返回 nil 表示整份 diff。
func Selection(total int, selected []int) ([]int, error) {
	if len(selected) == 0 {
		return nil, nil
	}
	seen := make(map[int]bool, len(selected))
	out := make([]int, 0, len(selected))
	for _, idx := range selected {
		if idx < 0 || idx >= total {
			return nil, fmt.Errorf("patch: 块下标 %d 超出范围（共 %d 块）", idx, total)
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, idx)
	}
	sort.Ints(out)
	if len(out) == total {
		return nil, nil // 全选等价于整份应用
	}
	return out, nil
}

// ApplyHunks 把 diff 里选中的块应用到 old 上，未选中的块保持旧内容。
// selected 为空表示应用全部块；调用方应保证 old 已统一成 LF 换行。
//
// 原文件末尾没有换行时，结果也不补换行（splitLines 会补一个，这里再剥掉）。
func ApplyHunks(old []byte, diff string, selected []int) ([]byte, error) {
	hunks, err := ParseHunks(diff)
	if err != nil {
		return nil, err
	}
	picked, err := Selection(len(hunks), selected)
	if err != nil {
		return nil, err
	}
	if picked == nil {
		// 全部选中时仍走同一条按块重放的路径：它同时验证了 diff 与 old 一致。
		picked = make([]int, 0, len(hunks))
		for i := range hunks {
			picked = append(picked, i)
		}
	}

	oldLines := splitLines(old)
	// 空文件没有「末尾换行」可言，结果完全按 diff 拼出来。
	trailing := len(old) == 0 || old[len(old)-1] == '\n'

	out := make([]string, 0, len(oldLines)+8)
	cursor := 0
	for _, idx := range picked {
		h := hunks[idx]
		start := h.OldStart - 1
		if h.OldCount == 0 {
			start = h.OldStart
		}
		if start < cursor {
			return nil, fmt.Errorf("patch: 块 %q 与前一个块重叠", h.Header)
		}
		if start > len(oldLines) {
			return nil, fmt.Errorf("patch: 块 %q 起点 %d 超出旧内容（%d 行）", h.Header, start, len(oldLines))
		}
		out = append(out, oldLines[cursor:start]...)

		pos := start
		for _, line := range h.Lines {
			switch line[0] {
			case ' ':
				if err := expectLine(h, oldLines, pos, line[1:]); err != nil {
					return nil, err
				}
				out = append(out, oldLines[pos])
				pos++
			case '-':
				if err := expectLine(h, oldLines, pos, line[1:]); err != nil {
					return nil, err
				}
				pos++
			case '+':
				out = append(out, line[1:])
			}
		}
		cursor = start + h.OldCount
	}
	out = append(out, oldLines[cursor:]...)

	joined := strings.Join(out, "")
	if !trailing {
		joined = strings.TrimSuffix(joined, "\n")
	}
	return []byte(joined), nil
}

func expectLine(h Hunk, lines []string, pos int, want string) error {
	if pos < 0 || pos >= len(lines) {
		return fmt.Errorf("patch: 块 %q 需要旧内容的第 %d 行，但文件只有 %d 行", h.Header, pos+1, len(lines))
	}
	if strings.TrimSuffix(lines[pos], "\n") != strings.TrimSuffix(want, "\n") {
		return fmt.Errorf("patch: 块 %q 的旧内容第 %d 行对不上（文件已变化）", h.Header, pos+1)
	}
	return nil
}
