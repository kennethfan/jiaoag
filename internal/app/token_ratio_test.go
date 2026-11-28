package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// usedProvider 每轮都回报一个明显偏大的 prompt_tokens，让估算系数被顶到上限，
// 便于断言「第二个任务的首轮估算变大了」。
type usedProvider struct{}

func (usedProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不走非流式路径")
}

func (usedProvider) ChatStream(_ context.Context, _ llm.Request, fn func(llm.Chunk) error) error {
	if err := fn(llm.Chunk{TextDelta: "好了"}); err != nil {
		return err
	}
	return fn(llm.Chunk{Done: true, StopReason: llm.StopEnd, Usage: &llm.Usage{PromptTokens: 9000, CompletionTokens: 2}})
}

func tokenRatioApp(t *testing.T) (*app.App, *store.Store) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "jiaoag.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	a := app.New(db, event.NewHub())
	t.Cleanup(func() { _ = a.Close() })

	err = a.ConfigureRuntime(app.RuntimeDeps{
		DefaultProvider: "openai",
		Provider: func(sess domain.Session) (llm.Provider, string, error) {
			model := sess.Model
			if model == "" {
				model = "gpt-x"
			}
			return usedProvider{}, model, nil
		},
		Tools: func(ws *workspace.Workspace, env tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		MaxRounds:     2,
		ContextBudget: 60000,
	})
	if err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}
	return a, db
}

// firstContextTokens 取某个任务第一轮 assistant.start 里的 estimates。
func firstContextTokens(t *testing.T, a *app.App, sessionID domain.SessionID) int {
	t.Helper()
	events, err := a.Replay(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for _, ev := range events {
		if ev.Type != domain.EventAssistantStart {
			continue
		}
		var p domain.AssistantStartPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("解 assistant.start: %v", err)
		}
		if p.Round == 1 && p.ContextTokens > 0 {
			return p.ContextTokens
		}
	}
	t.Fatal("没有找到第一轮的 assistant.start")
	return 0
}

func TestTokenRatioPersistsAndSeedsNextTask(t *testing.T) {
	a, db := tokenRatioApp(t)
	ctx := context.Background()
	sess, err := a.CreateSession(ctx, t.TempDir(), "openai", "gpt-x", "估算系数")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := a.SubmitTask(ctx, sess.ID, "第一次"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	waitTaskDone(t, a, sess.ID)
	firstCtx := firstContextTokens(t, a, sess.ID)

	// 系数应当已经按 provider/model 落库，并且因为真实用量远大于估算被顶到上限附近。
	ratio, err := a.TokenRatio(ctx, "openai/gpt-x")
	if err != nil {
		t.Fatalf("TokenRatio: %v", err)
	}
	if ratio <= 1.2 {
		t.Fatalf("真实用量远大于估算时系数应显著上移，得到 %.2f", ratio)
	}
	if samples, err := db.TokenRatioSamples(ctx, "openai/gpt-x"); err != nil || samples != 1 {
		t.Fatalf("应记录一次校准样本，得到 %d（err=%v）", samples, err)
	}

	// 第二个任务首轮直接用上次的系数：估算出的上下文占用应明显变大。
	if _, err := a.SubmitTask(ctx, sess.ID, "第二次"); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	waitTaskDone(t, a, sess.ID)
	secondCtx := firstContextTokensOfLast(t, a, sess.ID)

	if secondCtx <= firstCtx {
		t.Fatalf("第二个任务首轮应沿用上次系数：first=%d second=%d", firstCtx, secondCtx)
	}
	if samples, _ := db.TokenRatioSamples(ctx, "openai/gpt-x"); samples != 2 {
		t.Fatalf("第二个任务结束应再记一次样本，得到 %d", samples)
	}
}

// firstContextTokensOfLast 取最后一个任务的第一轮 assistant.start。
func firstContextTokensOfLast(t *testing.T, a *app.App, sessionID domain.SessionID) int {
	t.Helper()
	tasks, err := a.ListTasks(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("没有任务")
	}
	last := tasks[len(tasks)-1]
	events, err := a.Replay(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for _, ev := range events {
		if ev.Type != domain.EventAssistantStart || ev.TaskID != last.ID {
			continue
		}
		var p domain.AssistantStartPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("解 assistant.start: %v", err)
		}
		if p.Round == 1 && p.ContextTokens > 0 {
			return p.ContextTokens
		}
	}
	t.Fatal("最后一个任务没有第一轮 assistant.start")
	return 0
}
