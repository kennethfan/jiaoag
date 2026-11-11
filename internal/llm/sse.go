package llm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseMaxLineBytes 是单条 SSE data 的上限。整段工具参数可能很大，给足缓冲。
const sseMaxLineBytes = 4 * 1024 * 1024

// readSSE 逐条读取 Server-Sent Events。fn 返回 error 时立即中止。
//
// 两家的流式都是 SSE（Anthropic 额外带 event: 行），所以解析放在这里共用。
// provider 只用于给读取失败补上上下文：调用方拿到「哪一家断了」比裸的
// "unexpected EOF" 有用。
//
// 错误语义：只有「读流本身失败」才算 ErrTransient（ADR-0008 把网络错误归为
// 可重试）；fn 主动返回的错误必须原样透传，不能被包装成可重试——否则调用方
// 的取消会被重试装饰器当成网络抖动重发。
func readSSE(provider string, r io.Reader, fn func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), sseMaxLineBytes)

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
		// 单行超限是协议体量问题，重试也不会变小，按请求错误处理。
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("llm: %s 流式响应单行超过 %d 字节上限：%w", provider, sseMaxLineBytes, ErrBadRequest)
		}
		return fmt.Errorf("llm: %s 读取流式响应失败：%w: %w", provider, ErrTransient, err)
	}
	// 流末尾没有空行时，最后一条也要处理。
	return flush()
}
