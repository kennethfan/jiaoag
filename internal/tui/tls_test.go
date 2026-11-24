package tui

import (
	"net/http"
	"testing"
)

// -insecure 只改变 TLS 校验，不改变别的连接行为；默认必须走系统信任链。
func TestInsecureHTTPClientSkipsVerification(t *testing.T) {
	client := insecureHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型不对：%T", client.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("应设置 InsecureSkipVerify")
	}
}
