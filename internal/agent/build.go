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

// Build 是纯函数：把完整历史压进 token 预算，返回可直接发给供应商的消息。
//
// 裁剪单位是「用户轮」——从最旧的一条用户提问丢到下一个提问之前，这样不会
// 留下孤立的 tool_result（供应商会直接拒绝）。压缩摘要永远保留：它代表已经
// 被丢弃的历史，丢掉它等于把上下文清空。system 与最后一条 user 消息同理：
// 预算不够时宁可超一点，也不能丢用户当下的问题。
//
// budget 是消息部分可用的 token 估算值：调用方已经从总预算里扣掉系统提示与
// 工具清单的固定占用。估算是 wifi 级的近似值，见 tokenEstimator。
func Build(history []llm.Message, budget int) []llm.Message {
	return buildWith(history, budget, nil)
}

// buildWith 让 Run 把带校准的估算器传进来；est 为 nil 时用未校准的默认估算。
func buildWith(history []llm.Message, budget int, est *tokenEstimator) []llm.Message {
	msgs := trimToolResults(history)
	var pinned []llm.Message
	if len(msgs) > 0 && isCompactedMessage(msgs[0]) {
		pinned = msgs[:1:1]
		msgs = msgs[1:]
	}
	if budget > 0 {
		for countTokens(msgs, est)+countTokens(pinned, est) > budget {
			starts := promptStarts(msgs)
			if len(starts) < 2 {
				break
			}
			msgs = msgs[starts[1]:]
		}
	}
	if len(pinned) == 0 {
		return msgs
	}
	return append(pinned, msgs...)
}

// countTokens 估算一组消息占用的 token；est 为 nil 时用默认估算器。
func countTokens(msgs []llm.Message, est *tokenEstimator) int {
	if est == nil {
		est = newTokenEstimator(0)
	}
	return est.messages(msgs)
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

func promptStarts(msgs []llm.Message) []int {
	var out []int
	for i, msg := range msgs {
		if isPromptMessage(msg) {
			out = append(out, i)
		}
	}
	return out
}

// countChars 数字符数，只用于事件与审计里的可读指标（「压缩了多少字」），
// 预算判定一律走 token 估算。字符数与真实占用的比值不稳定，不可拿来做门槛。
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
