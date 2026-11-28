package agent

import (
	"math"
	"unicode"

	"github.com/kennethfan/jiaoag/internal/llm"
)

// 上下文预算按「估算 token」而不是字符数来算：字符数与真实占用的比值随内容
// 在 0.5～4 之间浮动（中文一字一 token，英文四字符一 token，代码与 JSON 的
// 标点又更贵），按字符配预算等于让同一个配置在不同任务上差出几倍。
//
// 估算必然有偏差——没有任何供应商提供可离线复现的分词器——所以除了按字符
// 类别加权，还用每轮供应商回报的 prompt_tokens 做在线校准：系数是
// 实际/估算的 EWMA，夹在 [0.5, 3] 之间，防止一次异常响应把预算带跑偏。
const (
	defaultTokenRatio = 1.0
	minTokenRatio     = 0.5
	maxTokenRatio     = 3.0
	// tokenRatioWeight 是每次校准吸收的新样本比重。
	tokenRatioWeight = 0.3

	// perMessageTokens / perBlockTokens 是消息与内容块的结构开销
	// （角色标记、分隔符、tool_use 的 id/name 包装等）。
	perMessageTokens = 4
	perBlockTokens   = 2

	// minCalibrationTokens 以下的请求不参与校准：几十个 token 的请求里，
	// 固定开销占比过大，比值没有参考价值。
	minCalibrationTokens = 200
)

// tokenEstimator 估算提示词占用，并用真实用量修正系数。
//
// 它不是并发安全的，但也不需要：每个任务在 Run 里创建一个。
//
// initial 是上一个任务结束时留下的系数（按 provider/model 落库，见
// store.TokenRatio）：新任务首轮就能用上历史校准，不必从 1.0 冷启动。
// 0 或越界视为没有历史，退回默认值。
type tokenEstimator struct {
	ratio   float64
	samples int
}

func newTokenEstimator(initial float64) *tokenEstimator {
	ratio := initial
	if ratio < minTokenRatio || ratio > maxTokenRatio {
		ratio = defaultTokenRatio
	}
	return &tokenEstimator{ratio: ratio}
}

// currentRatio 返回当前系数，供任务结束时回写。
func (e *tokenEstimator) currentRatio() float64 { return e.ratio }

// calibrated 表示本任务至少取到过一个可用的样本；没有样本时不覆盖历史值。
func (e *tokenEstimator) calibrated() bool { return e.samples > 0 }

// messages 估算消息历史（含结构开销），结果已按校准系数换算。
func (e *tokenEstimator) messages(msgs []llm.Message) int {
	raw := 0
	for _, msg := range msgs {
		raw += perMessageTokens + len(msg.Blocks)*perBlockTokens
		for _, block := range msg.Blocks {
			raw += estimateText(block.Text)
			raw += estimateText(block.ToolName)
			raw += estimateText(string(block.Args))
			raw += estimateText(block.Result)
		}
	}
	return e.scale(raw)
}

// overhead 估算系统提示与工具清单的固定占用。它每轮都要从预算里扣掉，
// 否则工具多的会话会在真正超限时才发现预算早就不够。
func (e *tokenEstimator) overhead(system string, tools []llm.ToolSpec) int {
	raw := estimateText(system)
	for _, tool := range tools {
		raw += estimateText(tool.Name) + estimateText(tool.Description) + estimateText(string(tool.Schema))
	}
	return e.scale(raw)
}

// observe 用一次真实请求的 prompt_tokens 校准系数。
func (e *tokenEstimator) observe(estimated, actual int) {
	if estimated < minCalibrationTokens || actual <= 0 {
		return
	}
	sample := float64(actual) / float64(estimated)
	next := e.ratio*(1-tokenRatioWeight) + sample*tokenRatioWeight
	if next < minTokenRatio {
		next = minTokenRatio
	}
	if next > maxTokenRatio {
		next = maxTokenRatio
	}
	e.ratio = next
	e.samples++
}

func (e *tokenEstimator) scale(raw int) int {
	if raw <= 0 {
		return 0
	}
	ratio := e.ratio
	if ratio <= 0 {
		ratio = defaultTokenRatio
	}
	return int(math.Ceil(float64(raw) * ratio))
}

// estimateText 按字符类别加权估算 token 数。
//
// 权重取自常见 BPE 分词器的经验值：ASCII 字母数字约 4 字符/token，标点与
// 符号更碎（约 2 字符/token），CJK 与假名/谚文约 1 字/token。拉丁扩展、
// emoji 之类取中间值。
func estimateText(s string) int {
	if s == "" {
		return 0
	}
	total := 0.0
	for _, r := range s {
		switch {
		case r < 0x80:
			if isWordByte(r) {
				total += 0.25
			} else {
				total += 0.5
			}
		case isCJK(r):
			total += 1
		default:
			total += 0.75
		}
	}
	if total < 1 {
		return 1
	}
	return int(math.Ceil(total))
}

func isWordByte(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		r == '_'
}

// isCJK 覆盖中日韩文字与假名、谚文：这些文字在主流分词器里基本一字一 token。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) ||
		unicode.Is(unicode.Bopomofo, r)
}
