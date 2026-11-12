package app_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// fakeOpenAI 按轮次返回预置的流式响应：glob → read_file → 纯文本。
func fakeOpenAI(t *testing.T, rounds *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("读取请求体: %v", err)
		}
		round := atomic.AddInt32(rounds, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		frames := sseFrames(round)
		for _, frame := range frames {
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

func sseFrames(round int32) []string {
	data := func(body string) string { return "data: " + body + "\n\n" }
	usage := data(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)

	switch round {
	case 1:
		return []string{
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_glob","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"**/*.go\"}"}}]},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
			usage,
			data(`[DONE]`),
		}
	case 2:
		return []string{
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
			usage,
			data(`[DONE]`),
		}
	default:
		return []string{
			data(`{"choices":[{"delta":{"content":"仓库根目录有"},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{"content":" main.go，里面是 package main。"},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
			usage,
			data(`[DONE]`),
		}
	}
}

func TestEndToEndReadOnlyLoop(t *testing.T) {
	ctx := context.Background()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var rounds int32
	srv := fakeOpenAI(t, &rounds)
	defer srv.Close()

	a := newApp(t)
	timeout, retries := 5, 0
	providerCfg := config.Provider{
		Name:           "fake",
		Type:           "openai",
		BaseURL:        srv.URL,
		Model:          "fake-model",
		APIKey:         config.Secret("test-key"),
		TimeoutSeconds: &timeout,
		MaxRetries:     &retries,
	}
	if err := a.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			p, err := llm.New(providerCfg)
			return p, providerCfg.Model, err
		},
		Tools: func(ws *workspace.Workspace) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws), tools.NewGrep(ws), tools.NewGlob(ws))
		},
		MaxRounds:     6,
		ContextBudget: 20000,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	sess, err := a.CreateSession(ctx, dir, "fake", providerCfg.Model, "端到端")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	task, err := a.SubmitTask(ctx, sess.ID, "这仓库根目录有哪些 Go 文件？main.go 里是什么？")
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	done := waitTerminal(t, a, task.ID)
	if done.Status != domain.TaskCompleted {
		t.Fatalf("终态 = %s，错误 = %q", done.Status, done.Error)
	}
	if done.PromptTokens == 0 || done.CompleteTokens == 0 {
		t.Fatalf("token 未累加：%+v", done)
	}

	events, err := a.Events(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	var (
		rounds_   int
		requested []string
		results   []string
		finalText string
		statusSeq []string
	)
	for _, ev := range events {
		switch ev.Type {
		case domain.EventRoundStarted:
			rounds_++
		case domain.EventToolRequested:
			var p domain.ToolRequestedPayload
			_ = ev.Decode(&p)
			requested = append(requested, p.Tool)
		case domain.EventToolResult:
			var p domain.ToolResultPayload
			_ = ev.Decode(&p)
			results = append(results, p.Output)
		case domain.EventAssistantMessage:
			var p domain.AssistantMessagePayload
			_ = ev.Decode(&p)
			finalText = p.Text
		case domain.EventTaskStatus:
			var p domain.TaskStatusPayload
			_ = ev.Decode(&p)
			statusSeq = append(statusSeq, fmt.Sprintf("%s→%s", p.From, p.To))
		}
	}

	if rounds_ != 3 {
		t.Fatalf("轮数 = %d，期望 3", rounds_)
	}
	if len(requested) != 2 || requested[0] != "glob" || requested[1] != "read_file" {
		t.Fatalf("工具调用序列 = %v，期望 [glob read_file]", requested)
	}
	if len(results) != 2 || !strings.Contains(results[0], "main.go") {
		t.Fatalf("glob 结果未带给模型：%v", results)
	}
	if !strings.Contains(results[1], "package main") {
		t.Fatalf("read_file 结果未带给模型：%v", results)
	}
	if !strings.Contains(finalText, "main.go") {
		t.Fatalf("最终回答 = %q", finalText)
	}
	if got := strings.Join(statusSeq, ","); !strings.Contains(got, "queued→running") || !strings.Contains(got, "running→completed") {
		t.Fatalf("状态迁移序列 = %v", statusSeq)
	}
}
