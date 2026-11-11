package agent

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/llm"
)

// 工具结果超过 8 KB 时折成「首 4 KB + 尾 2 KB」：头部给模型定位，尾部通常
// 是错误信息或结论。
const (
	toolResultLimit = 8 << 10
	toolResultHead  = 4 << 10
	toolResultTail  = 2 << 10
)

// Build 是纯函数：把完整历史压进预算，返回可直接发给供应商的消息。
//
// 裁剪单位是「轮」——从最旧的一条 user 消息丢到下一个 user 消息之前，
// 这样不会留下孤立的 tool_result（供应商会直接拒绝）。system 与最后一条
// user 消息永不丢：预算不够时宁可超一点，也不能丢用户当下的问题。
func Build(history []llm.Message, budget int) []llm.Message {
	msgs := trimToolResults(history)
	if budget <= 0 {
		return msgs
	}
	for countChars(msgs) > budget {
		starts := userStarts(msgs)
		if len(starts) < 2 {
			break
		}
		msgs = msgs[starts[1]:]
	}
	return msgs
}

// trimToolResults 截断过长工具结果，不改动入参。
func trimToolResults(history []llm.Message) []llm.Message {
	out := make([]llm.Message, len(history))
	for i, msg := range history {
		blocks := make([]llm.Block, len(msg.Blocks))
		copy(blocks, msg.Blocks)
		for j := range blocks {
			if blocks[j].Kind == llm.BlockToolResult && len(blocks[j].Result) > toolResultLimit {
				blocks[j].Result = middleTruncate(blocks[j].Result, toolResultHead, toolResultTail)
			}
		}
		out[i] = llm.Message{Role: msg.Role, Blocks: blocks}
	}
	return out
}

func userStarts(msgs []llm.Message) []int {
	var out []int
	for i, msg := range msgs {
		if msg.Role == llm.RoleUser {
			out = append(out, i)
		}
	}
	return out
}

// countChars 按 UTF-8 字符数计预算：无依赖、可测，token 精算留到 v2。
func countChars(msgs []llm.Message) int {
	total := 0
	for _, msg := range msgs {
		for _, block := range msg.Blocks {
			total += utf8.RuneCountInString(block.Text)
			total += utf8.RuneCountInString(block.ToolName)
			total += utf8.RuneCount(block.Args)
			total += utf8.RuneCountInString(block.Result)
		}
	}
	return total
}

func middleTruncate(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	start := runeBoundary(s, head)
	end := runeBoundary(s, len(s)-tail)
	return strings.TrimRight(s[:start], "\n") +
		"\n…（已截断 " + strconv.Itoa(len(s)-start-(len(s)-end)) + " 字节）…\n" +
		strings.TrimLeft(s[end:], "\n")
}

// runeBoundary 把字节下标回退到不会切断 UTF-8 字符的位置。
func runeBoundary(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
