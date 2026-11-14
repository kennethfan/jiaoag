package store_test

import (
	"net/url"
	"path/filepath"
	"strings"
)

// testDSN 把数据库路径转成 SQLite 能接受的 file URI。
//
// 与 store.Open 里的拼法保持一致：Windows 的盘符（C:\…）必须写成
// file:///C:/…，否则 SQLite 会把 "C:" 当成非法的 authority。
func testDSN(path string) string {
	slash := filepath.ToSlash(path)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}
	return (&url.URL{Scheme: "file", Path: slash}).String()
}
