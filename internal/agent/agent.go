// Package agent 是只读闭环的驱动：一轮轮调模型、执行工具、重放历史。
//
// 它不碰磁盘（只通过 workspace/tools）、不碰数据库（只通过 Recorder），
// 因此可以用假 Provider 与假 Recorder 单独测。三条不变量仍然由 app 与
// store 保证：事件日志是唯一历史来源，写入只走 Recorder。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/prompt"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	// DefaultMaxRounds 与 config 的 agent.max_rounds 默认值保持一致。
	DefaultMaxRounds = 24
	// DefaultContextBudget 是上下文预算（UTF-8 字符数）。
	DefaultContextBudget = 60000
	// minContextBudget 防止上下文超限重试把预算压到不可用的程度。
	minContextBudget = 2000
)

// Recorder 是 agent 对应用层的全部要求。
//
// 分开三个方法而不是一个「发事件」：Delta 是临时帧（不落库、不占 seq），
// Record 是原子迁移（落库 + 广播），Events 是只读回放。混在一起会让调用方
// 以为 delta 也会进日志。
type Recorder interface {
	Record(ctx context.Context, tr domain.Transition) error
	Events(ctx context.Context, sessionID domain.SessionID) ([]domain.Event, error)
	Delta(ctx context.Context, ev domain.Event) error
	// AwaitApproval 阻塞到审批被裁决。ctx 取消时返回 ctx.Err()：
	// 审批不设超时，等多久由用户决定，只有取消能把它打断。
	AwaitApproval(ctx context.Context, req ApprovalRequest) (ApprovalResult, error)
}

// ApprovalRequest 是一次等待裁决的请求。
//
// Tool/Args 用来识别 allow_session：同一会话里同一个 tool+args 只问一次。
// agent 把参数原文交给 Recorder，而不是让它去查工具调用表，是因为审批
// 只能由正在执行这次调用的协程发起，参数此刻就在手里。
type ApprovalRequest struct {
	SessionID  domain.SessionID
	ApprovalID domain.ApprovalID
	Tool       string
	Args       json.RawMessage
}

// ApprovalResult 是裁决结果。
//
// Source 区分人工与策略放行（allow_session 命中时是 policy），必须入审计。
type ApprovalResult struct {
	Decision domain.Decision
	Source   domain.ApprovalSource
	// Note 是用户裁决时留的说明，落进 approvals.note。
	Note string
}

// Options 是 Agent 的依赖与约束。
type Options struct {
	Provider      llm.Provider
	Tools         *tools.Registry
	Workspace     *workspace.Workspace
	Recorder      Recorder
	Model         string
	MaxRounds     int
	ContextBudget int
	SystemLayers  []prompt.Layer
	Logger        *slog.Logger

	// Ruleset 是命令白名单与收紧规则的不可变快照。
	Ruleset *perm.Ruleset
	// OutputDir 是命令全文日志的根目录；空表示不落全文。
	OutputDir string
}

// Agent 执行单个任务。多个任务可以并发使用同一个 Agent（无可变状态）。
type Agent struct {
	opts Options
}

// New 校验依赖并补齐默认值。
func New(opts Options) (*Agent, error) {
	switch {
	case opts.Provider == nil:
		return nil, errors.New("agent: 缺少 Provider")
	case opts.Tools == nil:
		return nil, errors.New("agent: 缺少工具集")
	case opts.Workspace == nil:
		return nil, errors.New("agent: 缺少工作区")
	case opts.Recorder == nil:
		return nil, errors.New("agent: 缺少 Recorder")
	}
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = DefaultMaxRounds
	}
	if opts.ContextBudget <= 0 {
		opts.ContextBudget = DefaultContextBudget
	}
	if len(opts.SystemLayers) == 0 {
		opts.SystemLayers = []prompt.Layer{prompt.Builtin()}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Ruleset == nil {
		opts.Ruleset = perm.DefaultRuleset()
	}
	return &Agent{opts: opts}, nil
}

// SystemPrompt 返回任务所用的系统提示词与指纹，供 SubmitTask 冻结版本。
func (a *Agent) SystemPrompt() (prompt.Result, error) {
	return a.system()
}

// Run 执行任务直到结束。终态（completed/failed/canceled）由这里记录。
//
// 返回 error 只表示「没能记录终态」这类基础设施故障；模型失败与工具失败
// 都以终态事件的形式落库，不向上抛。
func (a *Agent) Run(ctx context.Context, task domain.Task) error {
	system, err := a.system()
	if err != nil {
		// 终态必须落库：否则任务会停在 running，会话再也提交不了新任务。
		return a.finish(ctx, task, domain.TaskFailed, "internal_error", err.Error(), domain.TokenUsage{})
	}
	events, err := a.opts.Recorder.Events(ctx, task.SessionID)
	if err != nil {
		if ctx.Err() != nil {
			return a.finish(ctx, task, domain.TaskCanceled, "canceled", "", domain.TokenUsage{})
		}
		return a.finish(ctx, task, domain.TaskFailed, "internal_error",
			fmt.Sprintf("读取会话历史失败：%v", err), domain.TokenUsage{})
	}
	history := MessagesFromEvents(events)

	var usage domain.TokenUsage
	budget := a.opts.ContextBudget
	overflowRetried := false

	for round := 1; round <= a.opts.MaxRounds; round++ {
		if ctx.Err() != nil {
			return a.finish(ctx, task, domain.TaskCanceled, "canceled", "", usage)
		}
		started, err := domain.NewEvent(domain.EventRoundStarted, task.SessionID, task.ID,
			domain.RoundStartedPayload{Round: round})
		if err != nil {
			return a.finish(ctx, task, domain.TaskFailed, "internal_error", err.Error(), usage)
		}
		if err := a.record(ctx, task, domain.Transition{Events: []domain.Event{started}}); err != nil {
			return a.terminate(ctx, task, err, usage)
		}

		var (
			message llm.Message
			used    domain.TokenUsage
		)
		for {
			req := llm.Request{
				Model:    a.opts.Model,
				System:   system.Text,
				Messages: Build(history, budget),
				Tools:    a.toolSpecs(),
			}
			message, used, err = a.callModel(ctx, task, round, req)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return a.finish(ctx, task, domain.TaskCanceled, "canceled", "", usage)
			}
			if errors.Is(err, llm.ErrContextOverflow) && !overflowRetried {
				// 只收缩一次：再超限说明预算模型或历史本身有问题，重试也没用。
				overflowRetried = true
				budget /= 2
				if budget < minContextBudget {
					budget = minContextBudget
				}
				a.opts.Logger.Warn("上下文超限，收缩预算后重试", "task", task.ID, "budget", budget)
				continue
			}
			return a.terminate(ctx, task, err, usage)
		}
		usage.PromptTokens += used.PromptTokens
		usage.CompletionTokens += used.CompletionTokens
		history = append(history, message)

		calls := toolUses(message)
		if len(calls) == 0 {
			return a.finish(ctx, task, domain.TaskCompleted, "completed", "", usage)
		}

		results := make([]llm.Block, 0, len(calls))
		for _, call := range calls {
			block, err := a.runTool(ctx, task, round, call)
			if err != nil {
				if ctx.Err() != nil {
					return a.finish(ctx, task, domain.TaskCanceled, "canceled", "", usage)
				}
				return a.terminate(ctx, task, err, usage)
			}
			results = append(results, block)
		}
		history = append(history, llm.ToolResultMessage(results...))
	}

	return a.finish(ctx, task, domain.TaskFailed, "max_rounds",
		fmt.Sprintf("超过最大轮数 %d，任务未完成", a.opts.MaxRounds), usage)
}

// callModel 发起一轮模型调用，记录 assistant.start / 增量 / assistant.message。
func (a *Agent) callModel(ctx context.Context, task domain.Task, round int, req llm.Request) (llm.Message, domain.TokenUsage, error) {
	start, err := domain.NewEvent(domain.EventAssistantStart, task.SessionID, task.ID,
		domain.AssistantStartPayload{Round: round, Model: req.Model})
	if err != nil {
		// 事件构造失败是内部错误，不能让 terminate 归到 provider_error。
		return llm.Message{}, domain.TokenUsage{}, &recordError{err: err}
	}
	if err := a.record(ctx, task, domain.Transition{Events: []domain.Event{start}}); err != nil {
		return llm.Message{}, domain.TokenUsage{}, err
	}

	var (
		message llm.Message
		usage   domain.TokenUsage
		stop    llm.StopReason
	)

	streamer, streaming := a.opts.Provider.(llm.Streamer)
	if !streaming {
		resp, err := a.opts.Provider.Chat(ctx, req)
		if err != nil {
			return llm.Message{}, domain.TokenUsage{}, err
		}
		message = resp.Message
		stop = resp.StopReason
		usage = domain.TokenUsage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens}
	} else {
		var b strings.Builder
		acc := newToolAccumulator()

		err = streamer.ChatStream(ctx, req, func(c llm.Chunk) error {
			if c.TextDelta != "" {
				b.WriteString(c.TextDelta)
				a.publishDelta(ctx, task, c.TextDelta)
			}
			if c.ToolCallDelta != nil {
				acc.add(*c.ToolCallDelta)
			}
			if c.Usage != nil {
				usage = domain.TokenUsage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens}
			}
			if c.StopReason != "" {
				stop = c.StopReason
			}
			return nil
		})
		if err != nil {
			return llm.Message{}, domain.TokenUsage{}, err
		}

		blocks := make([]llm.Block, 0, 1+len(acc.order))
		if text := b.String(); text != "" {
			blocks = append(blocks, llm.Block{Kind: llm.BlockText, Text: text})
		}
		blocks = append(blocks, acc.blocks()...)
		message = llm.Message{Role: llm.RoleAssistant, Blocks: blocks}
	}

	payload := domain.AssistantMessagePayload{Round: round, StopReason: string(stop)}
	var text strings.Builder
	for _, block := range message.Blocks {
		switch block.Kind {
		case llm.BlockText:
			text.WriteString(block.Text)
		case llm.BlockToolUse:
			payload.ToolCalls = append(payload.ToolCalls, domain.AssistantToolCallPayload{
				ID:   block.ToolCallID,
				Name: block.ToolName,
				Args: block.Args,
			})
		}
	}
	payload.Text = text.String()

	ev, err := domain.NewEvent(domain.EventAssistantMessage, task.SessionID, task.ID, payload)
	if err != nil {
		return llm.Message{}, domain.TokenUsage{}, err
	}
	if err := a.record(ctx, task, domain.Transition{Events: []domain.Event{ev}}); err != nil {
		// 同上：写事件失败与模型无关，不能被标成 provider_error。
		return llm.Message{}, domain.TokenUsage{}, &recordError{err: err}
	}
	return message, usage, nil
}

// runTool 执行一次工具调用：requested → prepared →（写入走审批）→ result。
//
// 写入工具在这里被拦住：Prepare 已经算好 diff，但没有用户裁决就不落盘。
func (a *Agent) runTool(ctx context.Context, task domain.Task, round int, block llm.Block) (llm.Block, error) {
	call := tools.Call{ID: block.ToolCallID, Name: block.ToolName, Args: block.Args}
	started := time.Now()

	// toolCall 是这次调用的投影。存储层的 upsert 用 ON CONFLICT 覆盖全部
	// 字段，所以每一步都要整份写回：只带增量的写法会把 name/args 清空。
	toolCall := &domain.ToolCall{
		ID:        domain.ToolCallID(call.ID),
		TaskID:    task.ID,
		SessionID: task.SessionID,
		Round:     round,
		Name:      call.Name,
		Args:      call.Args,
		Status:    domain.ToolRequested,
		CreatedAt: domain.Now(),
	}

	requested, err := domain.NewEvent(domain.EventToolRequested, task.SessionID, task.ID,
		domain.ToolRequestedPayload{Tool: call.Name, ToolCallID: call.ID, Args: call.Args})
	if err != nil {
		return llm.Block{}, err
	}
	if err := a.record(ctx, task, domain.Transition{
		ToolCall: toolCall,
		Events:   []domain.Event{requested},
	}); err != nil {
		return llm.Block{}, err
	}

	prepared, perr := a.opts.Tools.Prepare(ctx, call)
	// tool.prepared 在准备之后立刻落库：即使后面被拒绝，事后也能看出它
	// 想调什么、想改哪些文件。
	if err := a.recordPrepared(ctx, task, toolCall, prepared); err != nil {
		return llm.Block{}, err
	}

	decision := a.opts.Ruleset.Evaluate(ctx, prepared)

	var (
		result tools.Result
		audits []domain.AuditRecord
	)
	switch {
	case perr != nil:
		result, err = tools.Failure(ctx, perr)
	case prepared.Change != nil:
		// 写入：先落提案与审批请求，等用户裁决。
		waitStart := time.Now()
		result, err = a.proposeChange(ctx, task, toolCall, prepared)
		// 人工审阅不是工具耗时：把等待的这段时间从计时里剔除，否则
		// 用户离开十分钟再批准，tool.result 会写一个 600000 ms。
		started = started.Add(time.Since(waitStart))
	case decision.Verdict == perm.Allow:
		if prepared.Command != nil {
			prepared.LogPath = a.logPath(task, toolCall.ID)
			// 免审批的命令同样要显式进入 running：执行可能跑很久，投影不能
			// 停在 prepared（那会被读成「还没人要它跑」）。
			if rerr := a.markCommandRunning(ctx, task, toolCall); rerr != nil {
				err = rerr
				break
			}
		}
		result, err = a.opts.Tools.Execute(ctx, prepared)
		if err == nil && prepared.Command != nil {
			audits = append(audits, execAudit(auditExecAllow, "policy", prepared, result, decision))
		}
	case prepared.Command != nil:
		// 命令：先落审批请求，等用户看到真实 argv 与 cwd 再决定。
		waitStart := time.Now()
		result, audits, err = a.proposeCommand(ctx, task, toolCall, prepared, decision)
		started = started.Add(time.Since(waitStart))
	default:
		// 判定要求审批，但工具没给出可审的东西（没有 diff、没有命令）：
		// 这是策略拒绝，不是工具失败。记成 denied 并留下审计，让「谁拒绝的、
		// 为什么拒绝」在日志里可回答。
		note := fmt.Sprintf("工具 %s 需要审批但没有提供可审的内容，已拒绝执行（%s）", call.Name, decision.Reason)
		toolCall.Status = domain.ToolDenied
		result = tools.Result{Content: note, IsError: true}
		audits = append(audits, domain.AuditRecord{
			Actor:      string(domain.SourcePolicy),
			Action:     auditToolDenied,
			ToolCallID: toolCall.ID,
			Detail:     rejectedDetail("", note),
		})
	}
	// 任何一步返回错误（最常见的是执行中被取消）都不能让调用就此消失：
	// abortTool 会补上终态与 tool.result。
	if err != nil {
		return a.abortTool(ctx, task, toolCall, prepared, decision, started, err)
	}
	return a.finishTool(ctx, task, toolCall, result, started, audits)
}

// recordError 标记「写事件失败」。存储故障不能和模型错误混为一谈：
// 报成 provider_error 会把排查方向带到供应商那边去。
type recordError struct{ err error }

func (e recordError) Error() string { return e.err.Error() }
func (e recordError) Unwrap() error { return e.err }

// terminate 把 Run 里任意中断映射成终态。**任何**失败都必须走到这里或
// finish：任务停在 running 会让会话永远处于忙状态。
//
// 先看 ctx：用户已经取消时，「已取消」比「内部错误」更接近事实。
func (a *Agent) terminate(ctx context.Context, task domain.Task, cause error, usage domain.TokenUsage) error {
	if ctx.Err() != nil {
		return a.finish(ctx, task, domain.TaskCanceled, "canceled", "", usage)
	}
	var re recordError
	if errors.As(cause, &re) {
		return a.finish(ctx, task, domain.TaskFailed, "internal_error", cause.Error(), usage)
	}
	return a.fail(ctx, task, cause, usage)
}

// fail 把模型层错误映射成终态。
func (a *Agent) fail(ctx context.Context, task domain.Task, cause error, usage domain.TokenUsage) error {
	code := "provider_error"
	switch {
	case errors.Is(cause, llm.ErrAuth):
		code = "provider_auth"
	case errors.Is(cause, llm.ErrBadRequest):
		code = "provider_bad_request"
	case errors.Is(cause, llm.ErrRateLimited):
		code = "rate_limited"
	case errors.Is(cause, llm.ErrContextOverflow):
		code = "context_overflow"
	}
	return a.finish(ctx, task, domain.TaskFailed, code, cause.Error(), usage)
}

// finish 记录终态。它用不随父 ctx 取消的 context 写库——用户按了取消，
// 「已取消」这条事实仍然必须落库，否则任务会永远停在 running。
func (a *Agent) finish(ctx context.Context, task domain.Task, to domain.TaskStatus, code, message string, usage domain.TokenUsage) error {
	ctx = context.WithoutCancel(ctx)
	ev, err := domain.NewEvent(domain.EventTaskStatus, task.SessionID, task.ID, domain.TaskStatusPayload{
		From:      domain.TaskRunning,
		To:        to,
		Reason:    code,
		Error:     message,
		PromptTok: usage.PromptTokens,
		OutputTok: usage.CompletionTokens,
	})
	if err != nil {
		return err
	}
	return a.record(ctx, task, domain.Transition{
		To:     to,
		Reason: message,
		Usage:  &usage,
		Events: []domain.Event{ev},
	})
}

func (a *Agent) record(ctx context.Context, task domain.Task, tr domain.Transition) error {
	tr.TaskID = task.ID
	if err := a.opts.Recorder.Record(ctx, tr); err != nil {
		return recordError{err: err}
	}
	return nil
}

// publishDelta 尽力投递临时帧：投不出去只影响界面的即时感，不影响正确性，
// 因此不返回错误也不中断对话。
func (a *Agent) publishDelta(ctx context.Context, task domain.Task, text string) {
	ev, err := domain.NewEvent(domain.EventAssistantDelta, task.SessionID, task.ID,
		domain.AssistantDeltaPayload{Text: text})
	if err != nil {
		return
	}
	if err := a.opts.Recorder.Delta(ctx, ev); err != nil {
		a.opts.Logger.Debug("投递增量失败", "task", task.ID, "err", err)
	}
}

func (a *Agent) system() (prompt.Result, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "sh"
	}
	vars := map[string]string{
		"workspace": a.opts.Workspace.Root(),
		"os":        runtime.GOOS,
		"shell":     shell,
		"date":      domain.Now().Format("2006-01-02"),
		"model":     a.opts.Model,
		"tools":     toolsSection(a.opts.Tools.Specs()),
	}
	return prompt.Assemble(a.opts.SystemLayers, vars)
}

// toolSpecs 把工具描述转成供应商能懂的形状。tools 包不 import llm，
// 这层转换是依赖方向的要求，不是多余的样板。
func (a *Agent) toolSpecs() []llm.ToolSpec {
	specs := a.opts.Tools.Specs()
	out := make([]llm.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, llm.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		})
	}
	return out
}

func toolsSection(specs []tools.Spec) string {
	var b strings.Builder
	for _, spec := range specs {
		fmt.Fprintf(&b, "- %s：%s\n", spec.Name, spec.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// toolUses 取出消息里的工具调用块，按到达顺序返回。
func toolUses(msg llm.Message) []llm.Block {
	var out []llm.Block
	for _, block := range msg.Blocks {
		if block.Kind == llm.BlockToolUse {
			out = append(out, block)
		}
	}
	return out
}

// toolAccumulator 按 index 合并流式工具调用分片。
type toolAccumulator struct {
	order   []int
	byIndex map[int]*llm.ToolCallDelta
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{byIndex: make(map[int]*llm.ToolCallDelta)}
}

func (t *toolAccumulator) add(delta llm.ToolCallDelta) {
	cur, ok := t.byIndex[delta.Index]
	if !ok {
		cur = &llm.ToolCallDelta{Index: delta.Index}
		t.byIndex[delta.Index] = cur
		t.order = append(t.order, delta.Index)
	}
	if delta.ID != "" && cur.ID == "" {
		cur.ID = delta.ID
	}
	if delta.Name != "" && cur.Name == "" {
		cur.Name = delta.Name
	}
	cur.ArgsFragment += delta.ArgsFragment
}

func (t *toolAccumulator) blocks() []llm.Block {
	order := append([]int(nil), t.order...)
	sort.Ints(order)

	out := make([]llm.Block, 0, len(order))
	for _, index := range order {
		call := t.byIndex[index]
		args := json.RawMessage(call.ArgsFragment)
		if len(args) == 0 || !json.Valid(args) {
			// 模型给了残缺 JSON：留空对象，让工具层报参数错误，而不是让
			// 上层的请求因为非法 JSON 直接 400。
			args = json.RawMessage(`{}`)
		}
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		out = append(out, llm.Block{
			Kind:       llm.BlockToolUse,
			ToolCallID: id,
			ToolName:   call.Name,
			Args:       args,
		})
	}
	return out
}
