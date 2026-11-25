package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// 页面本身公开（没有秘密），数据接口必须带令牌。
func TestWebPageIsPublicButInfoRequiresToken(t *testing.T) {
	ts := newTestServer(t)

	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("取首页: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("首页应匿名可读，得到 %d", res.StatusCode)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("首页 Content-Type: %q", res.Header.Get("Content-Type"))
	}

	res2, err := http.Get(ts.URL + "/api/info")
	if err != nil {
		t.Fatalf("取 info: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("不带令牌应 401，得到 %d", res2.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/info", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("带令牌取 info: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("带令牌应 200，得到 %d", res3.StatusCode)
	}
}

// 浏览器的 WebSocket 无法设置请求头，所以握手要能用 ?token=。
func TestWebSocketAcceptsQueryToken(t *testing.T) {
	ts := newTestServer(t)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=" + testToken
	conn, _, err := websocket.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("用查询串令牌握手: %v", err)
	}
	defer conn.CloseNow()

	env := readEnvelope(t, conn)
	if env.Type != "hello" {
		t.Fatalf("首帧应为 hello，得到 %q", env.Type)
	}

	// 错的令牌仍然要被拒。
	bad := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=wrong"
	if conn2, _, err := websocket.Dial(t.Context(), bad, nil); err == nil {
		conn2.CloseNow()
		t.Fatal("错误令牌不应该握手成功")
	}
}
