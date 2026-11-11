package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// readJSONBody 读取并解析请求体，失败时把原文带进报错。
func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("读取请求体失败：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v\n%s", err, body)
	}
	return m
}

// writeSSE 写出完整的 SSE 响应。
func writeSSE(t *testing.T, w http.ResponseWriter, events ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		if _, err := io.WriteString(w, e); err != nil {
			t.Errorf("写 SSE 失败：%v", err)
			return
		}
	}
}

// mergedCall 是测试侧的「工具调用拼装结果」。
type mergedCall struct {
	ID   string
	Name string
	Args string
}

// mergeToolCalls 按序号合并 ToolCallDelta，模拟调用方（M3 的 agent）的用法。
func mergeToolCalls(chunks []Chunk) []mergedCall {
	byIndex := make(map[int]*mergedCall)
	var order []int
	for _, c := range chunks {
		d := c.ToolCallDelta
		if d == nil {
			continue
		}
		m, ok := byIndex[d.Index]
		if !ok {
			m = &mergedCall{}
			byIndex[d.Index] = m
			order = append(order, d.Index)
		}
		if d.ID != "" {
			m.ID = d.ID
		}
		if d.Name != "" {
			m.Name = d.Name
		}
		m.Args += d.ArgsFragment
	}
	out := make([]mergedCall, 0, len(order))
	for _, i := range order {
		out = append(out, *byIndex[i])
	}
	return out
}

func collectStream(t *testing.T, s Streamer, req Request) ([]Chunk, error) {
	t.Helper()
	var chunks []Chunk
	err := s.ChatStream(context.Background(), req, func(c Chunk) error {
		chunks = append(chunks, c)
		return nil
	})
	return chunks, err
}

func joinedText(chunks []Chunk) string {
	var out string
	for _, c := range chunks {
		out += c.TextDelta
	}
	return out
}

func countDone(chunks []Chunk) int {
	n := 0
	for _, c := range chunks {
		if c.Done {
			n++
		}
	}
	return n
}

// countUsage 统计携带 usage 的 chunk 数：流式里 usage 只应出现一次（在 Done 上）。
func countUsage(chunks []Chunk) int {
	n := 0
	for _, c := range chunks {
		if c.Usage != nil {
			n++
		}
	}
	return n
}

func finalChunk(chunks []Chunk) Chunk {
	if len(chunks) == 0 {
		return Chunk{}
	}
	return chunks[len(chunks)-1]
}

func f64(v float64) *float64 { return &v }
