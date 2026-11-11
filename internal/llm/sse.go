package llm

import (
	"bufio"
	"io"
	"strings"
)

// readSSE 逐条读取 Server-Sent Events。fn 返回 error 时立即中止。
//
// 两家的流式都是 SSE（Anthropic 额外带 event: 行），所以解析放在这里共用。
func readSSE(r io.Reader, fn func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	// 单条 data 可能很大（整段工具参数），给足缓冲。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		event string
		data  []byte
		has   bool
	)

	flush := func() error {
		if !has {
			return nil
		}
		err := fn(event, data)
		event, data, has = "", nil, false
		return err
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// 注释或心跳，忽略。
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			piece := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if has {
				data = append(data, '\n')
			}
			data = append(data, piece...)
			has = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// 流末尾没有空行时，最后一条也要处理。
	return flush()
}
