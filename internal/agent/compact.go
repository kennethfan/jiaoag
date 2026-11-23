package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
)

// 上下文压缩的固定策略：超过预算的 80% 就把较早的轮次压成一段摘要，
// 保留最近 2 轮。参数不做成逐项配置——真正需要调的是开关。
const (
	// DefaultCompactKeepRounds 是压缩后保留的最近轮次数。
	DefaultCompactKeepRounds = 2
	// compactTriggerPercent 是触发压缩的预算占用比例。
	compactTriggerPercent = 80
	// compactTimeout 是摘要调用自己的超时；父 ctx 取消仍会立即生效。
	compactTimeout = 90 * time.Second
	// compactMaxTokens 限制摘要长度，避免压缩调用本身吃掉预算。
	compactMaxTokens = 1024
	// compactTranscriptChars 是喂给摘要模型的历史上限（字节）。
	compactTranscriptChars = 48 << 10
	// compactToolResultChars 是单条工具结果在摘要输入里的上限。
	compactToolResultChars = 2 << 10
	// minCompactOldChars 防止对一小段摘要反复做无意义的压缩。
	minCompactOldChars = 2000
	// compactCooldownRounds 是两次压缩之间的最小轮次间隔：压缩后历史通常已经
	// 回到预算内，连着再压只会把同一段摘要反复重写，白花调用。
	compactCooldownRounds = 3

	// compactedPrefix 标记摘要消息。它既要让 Build 固定保留，也要让
	// compactSplit 不把摘要当成一轮普通用户输入。
	compactedPrefix = "[早前对话摘要]"

	auditContextCompact       = "context.compact"
	auditContextCompactFailed = "context.compact_failed"
)

// CompactSummary 是一次摘要调用的结果。
type CompactSummary struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	Duration         time.Duration
}

// Compactor 把一段较早的历史压成摘要。抽成接口是为了让测试不依赖真实模型，
// 也为了将来换成更便宜的模型。
type Compactor interface {
	Compact(ctx context.Context, history []llm.Message) (CompactSummary, error)
}

// providerCompactor 用会话当前的 provider 做非流式摘要调用。
type providerCompactor struct {
	provider llm.Provider
	model    string
}

const compactSystemPrompt = `你是对话压缩器。把给定的历史对话压成一份供后续继续工作的摘要。

必须保留：
- 用户的目标、约束与偏好；
- 已经确认的决策；
- 改过或读过的关键文件、关键命令与结果；
- 遇到的错误、失败尝试与结论；
- 尚未完成的步骤与下一步。

不要编造，不要复述无关细节，不要输出寒暄。用简洁的中文分点输出，总长不超过 800 字。`

func (c providerCompactor) Compact(ctx context.Context, history []llm.Message) (CompactSummary, error) {
	if c.provider == nil {
		return CompactSummary{}, errors.New("agent: 没有可用的摘要 provider")
	}
	start := time.Now()
	resp, err := c.provider.Chat(ctx, llm.Request{
		Model:     c.model,
		System:    compactSystemPrompt,
		Messages:  []llm.Message{llm.TextMessage(llm.RoleUser, compactTranscript(history))},
		MaxTokens: compactMaxTokens,
	})
	if err != nil {
		return CompactSummary{}, err
	}
	return CompactSummary{
		Text:             messageText(resp.Message),
		Model:            c.model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		Duration:         time.Since(start),
	}, nil
}

// maybeCompact 在历史超出阈值时压缩较早部分。它永不返回错误：压缩是优化，
// 失败只记日志与审计，任务继续走 Build 的裁剪路径。
func (a *Agent) maybeCompact(ctx context.Context, task domain.Task, history *[]llm.Message, usage *domain.TokenUsage, budget, round int, lastCompact *int) {
	if !a.opts.Compact || a.opts.Compactor == nil {
		return
	}
	if *lastCompact > 0 && round-*lastCompact < compactCooldownRounds {
		return
	}
	msgs := *history
	if len(msgs) == 0 {
		return
	}
	threshold := budget * compactTriggerPercent / 100
	if threshold <= 0 || countChars(msgs) <= threshold {
		return
	}
	keep := a.opts.CompactKeepRounds
	if keep <= 0 {
		keep = DefaultCompactKeepRounds
	}
	split := compactSplit(msgs, keep)
	if split <= 0 || split >= len(msgs) {
		return
	}
	old := msgs[:split]
	before := countChars(old)
	if before < minCompactOldChars {
		return
	}
	// 先记下尝试时间：即使失败也不要在下一轮立刻重试，避免把摘要服务拖垮。
	*lastCompact = round

	cctx, cancel := context.WithTimeout(ctx, compactTimeout)
	defer cancel()
	summary, err := a.opts.Compactor.Compact(cctx, old)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		a.opts.Logger.Warn("上下文压缩失败，继续用裁剪后的历史", "task", task.ID, "err", err)
		a.recordCompactFailure(ctx, task, before, err)
		return
	}
	text := strings.TrimSpace(summary.Text)
	if text == "" {
		a.opts.Logger.Warn("上下文压缩返回空摘要，继续用裁剪后的历史", "task", task.ID)
		a.recordCompactFailure(ctx, task, before, errors.New("摘要为空"))
		return
	}

	msg := compactedMessage(text, len(old))
	*history = append([]llm.Message{msg}, msgs[split:]...)

	delta := domain.TokenUsage{
		PromptTokens:     summary.PromptTokens,
		CompletionTokens: summary.CompletionTokens,
	}
	usage.PromptTokens += delta.PromptTokens
	usage.CompletionTokens += delta.CompletionTokens

	ev, err := domain.NewEvent(domain.EventContextCompacted, task.SessionID, task.ID,
		domain.ContextCompactedPayload{
			Summary:          text,
			CoveredMessages:  len(old),
			BeforeChars:      before,
			SummaryChars:     utf8.RuneCountInString(text),
			Model:            summary.Model,
			PromptTokens:     summary.PromptTokens,
			CompletionTokens: summary.CompletionTokens,
			DurationMS:       summary.Duration.Milliseconds(),
		})
	if err != nil {
		a.opts.Logger.Warn("构造上下文压缩事件失败", "task", task.ID, "err", err)
		return
	}
	tr := domain.Transition{
		Usage:  &delta,
		Audit:  []domain.AuditRecord{compactAudit(auditContextCompact, before, summary, text, nil)},
		Events: []domain.Event{ev},
	}
	if err := a.record(ctx, task, tr); err != nil {
		// 内存里已经压缩；事件没落库只影响下次重放，不能让任务失败。
		a.opts.Logger.Warn("记录上下文压缩事件失败", "task", task.ID, "err", err)
	}
}

// recordCompactFailure 尽力记录失败审计。审计写不进去也只是少一条证据。
func (a *Agent) recordCompactFailure(ctx context.Context, task domain.Task, before int, cause error) {
	tr := domain.Transition{Audit: []domain.AuditRecord{
		compactAudit(auditContextCompactFailed, before, CompactSummary{}, "", cause),
	}}
	if err := a.record(ctx, task, tr); err != nil {
		a.opts.Logger.Warn("记录上下文压缩失败审计失败", "task", task.ID, "err", err)
	}
}

type compactAuditDetail struct {
	BeforeChars      int    `json:"before_chars"`
	SummaryChars     int    `json:"summary_chars,omitempty"`
	Model            string `json:"model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	Error            string `json:"error,omitempty"`
}

func compactAudit(action string, before int, summary CompactSummary, text string, cause error) domain.AuditRecord {
	detail := compactAuditDetail{
		BeforeChars:      before,
		SummaryChars:     utf8.RuneCountInString(text),
		Model:            summary.Model,
		PromptTokens:     summary.PromptTokens,
		CompletionTokens: summary.CompletionTokens,
		DurationMS:       summary.Duration.Milliseconds(),
	}
	if cause != nil {
		detail.Error = cause.Error()
	}
	raw, _ := json.Marshal(detail)
	return domain.AuditRecord{
		Actor:  "system",
		Action: action,
		Detail: raw,
	}
}

// compactSplit 返回「从这里开始保留」的下标；0 表示没有可压缩的较早内容。
//
// 优先按用户轮次切（跨任务的天然边界）；任务内部没有新的用户消息时，退到
// 助手消息边界。两处边界都不会切开 tool_use / tool_result 配对。
func compactSplit(msgs []llm.Message, keep int) int {
	if keep <= 0 {
		keep = DefaultCompactKeepRounds
	}
	var users, assistants []int
	for i, msg := range msgs {
		switch {
		case isCompactedMessage(msg):
			// 摘要不是一轮用户输入，不参与保留窗口计数。
		case isPromptMessage(msg):
			users = append(users, i)
		case msg.Role == llm.RoleAssistant:
			assistants = append(assistants, i)
		}
	}
	if len(users) > keep {
		return users[len(users)-keep]
	}
	if len(assistants) > keep {
		return assistants[len(assistants)-keep]
	}
	return 0
}

// isPromptMessage 判断一条消息是不是「用户提问」，而不是工具结果的 user 侧载体。
func isPromptMessage(msg llm.Message) bool {
	if msg.Role != llm.RoleUser {
		return false
	}
	for _, block := range msg.Blocks {
		if block.Kind == llm.BlockToolResult {
			return false
		}
	}
	return true
}

func compactedMessage(summary string, covered int) llm.Message {
	text := compactedPrefix + fmt.Sprintf("（已压缩 %d 条较早消息）\n", covered) + strings.TrimSpace(summary)
	return llm.TextMessage(llm.RoleUser, text)
}

func isCompactedMessage(msg llm.Message) bool {
	if msg.Role != llm.RoleUser || len(msg.Blocks) == 0 {
		return false
	}
	block := msg.Blocks[0]
	return block.Kind == llm.BlockText && strings.HasPrefix(block.Text, compactedPrefix)
}

// compactTranscript 把历史渲染成给摘要模型的纯文本。
func compactTranscript(msgs []llm.Message) string {
	var b strings.Builder
	for _, msg := range msgs {
		role := "用户"
		if msg.Role == llm.RoleAssistant {
			role = "助手"
		}
		for _, block := range msg.Blocks {
			switch block.Kind {
			case llm.BlockText:
				fmt.Fprintf(&b, "\n%s：%s\n", role, block.Text)
			case llm.BlockToolUse:
				fmt.Fprintf(&b, "\n助手调用工具 %s：%s\n", block.ToolName, clipRunes(string(block.Args), compactToolResultChars))
			case llm.BlockToolResult:
				fmt.Fprintf(&b, "\n工具 %s 结果：%s\n", block.ToolName, clipRunes(block.Result, compactToolResultChars))
			}
		}
	}
	s := strings.TrimSpace(b.String())
	if len(s) > compactTranscriptChars {
		s = middleTruncate(s, compactTranscriptChars*2/3, compactTranscriptChars/3)
	}
	return s
}

// clipRunes 按字节上限截断，但不切断 UTF-8 字符。
func clipRunes(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return strings.TrimRight(s[:runeBoundary(s, limit)], "\n") + "\n…（已截断）"
}

func messageText(msg llm.Message) string {
	var b strings.Builder
	for _, block := range msg.Blocks {
		if block.Kind == llm.BlockText {
			b.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
