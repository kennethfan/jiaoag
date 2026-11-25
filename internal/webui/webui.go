// Package webui 托管浏览器端界面：一份手写 HTML/JS/CSS，编译期嵌进二进制。
//
// 它不做任何业务判断——页面通过 WebSocket 说同一套协议（internal/protocol），
// 与 TUI 完全对等：审批、事件订阅、会话管理都走服务端已有的 handler。这样
// 前端只需要抄一份协议常量，不需要第二套 API，也不会出现「页面能做的事
// 服务端不认」。
//
// 没有构建步骤：浏览器直接加载 ES modules，改完刷新即生效。代价是不能用
// JSX/打包器，但单用户、单页面，够用。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets
var assets embed.FS

// Assets 返回内嵌的静态文件树，供测试与 FileServer 使用。
func Assets() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// 只可能是 embed 指令写错（构建期问题），不该在运行时发生。
		panic("webui: 内嵌资源缺失: " + err.Error())
	}
	return sub
}

// Handler 提供页面与静态资源。只暴露固定的几个文件：这个包不打算做通用
// 静态目录，未知路径一律 404，免得把内嵌树当成可枚举的目录服务。
func Handler() http.Handler {
	files := http.FileServerFS(Assets())
	known := map[string]bool{
		"/":            true,
		"/index.html":  true,
		"/app.css":     true,
		"/protocol.js": true,
		"/net.js":      true,
		"/store.js":    true,
		"/ui.js":       true,
		"/main.js":     true,
		"/favicon.svg": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !known[path] {
			http.NotFound(w, r)
			return
		}
		// 页面与脚本每次都要拿到最新的：开发时改完刷新即可，不必清缓存；
		// 单机使用，带宽不是问题。
		w.Header().Set("Cache-Control", "no-cache")
		if path == "/" {
			if strings.Contains(r.Header.Get("Accept"), "text/html") || r.Header.Get("Accept") == "" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			}
		}
		files.ServeHTTP(w, r)
	})
}
