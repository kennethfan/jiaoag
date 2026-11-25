package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestHandlerServesPageAndAssets(t *testing.T) {
	h := Handler()
	cases := []struct {
		path string
		code int
		ct   string
		body string
	}{
		{"/", http.StatusOK, "text/html", "<!doctype html>"},
		{"/app.css", http.StatusOK, "text/css", "--accent"},
		{"/main.js", http.StatusOK, "javascript", "sessionCreate"},
		{"/favicon.svg", http.StatusOK, "", "<svg"},
		{"/nope.js", http.StatusNotFound, "", ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Fatalf("%s: 状态码 %d，期望 %d", c.path, rec.Code, c.code)
		}
		if c.ct != "" && !strings.Contains(rec.Header().Get("Content-Type"), c.ct) {
			t.Fatalf("%s: Content-Type %q 不含 %q", c.path, rec.Header().Get("Content-Type"), c.ct)
		}
		if c.body != "" && !strings.Contains(rec.Body.String(), c.body) {
			t.Fatalf("%s: 正文不含 %q", c.path, c.body)
		}
	}
}

// 前端抄的是服务端常量：两边漂移不会有编译错误，只会在某个操作上静默
// 报 unsupported_type。这里把 JS 里出现过的每个协议字符串与 Go 侧对比。
func TestProtocolStringsMatchServer(t *testing.T) {
	goTypes := constantValues(t, "../../internal/protocol/protocol.go", `Type\w+\s*=\s*"([^"]+)"`)
	goKinds := constantValues(t, "../../internal/domain/events.go", `Event\w+\s+EventKind\s*=\s*"([^"]+)"`)
	goStates := constantValues(t, "../../internal/domain/state.go", `Task\w+\s+TaskStatus\s*=\s*"([^"]+)"`)

	js := readFile(t, "assets/protocol.js")
	for _, m := range regexp.MustCompile(`:\s*'([^']+)'`).FindAllStringSubmatch(js, -1) {
		value := m[1]
		if goTypes[value] || goKinds[value] || goStates[value] {
			continue
		}
		t.Errorf("protocol.js 里的 %q 在服务端找不到对应常量", value)
	}
	// 反向抽查：TUI 依赖的关键类型必须在前端有名字，否则页面会漏功能。
	for _, want := range []string{"session.manage", "approval.resolve", "event.snapshot", "task.submit"} {
		if !strings.Contains(js, "'"+want+"'") {
			t.Errorf("protocol.js 缺少 %q", want)
		}
	}
}

func constantValues(t *testing.T, path, pattern string) map[string]bool {
	t.Helper()
	src := readFile(t, path)
	out := make(map[string]bool)
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s 里没解析到常量（正则 %s）", path, pattern)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}
