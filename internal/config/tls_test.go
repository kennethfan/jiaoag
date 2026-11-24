package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/tlstest"
)

func TestServerTLSPairValidation(t *testing.T) {
	cert, key := tlstest.Pair(t)

	t.Run("都不配表示明文", func(t *testing.T) {
		if err := validConfig().ValidateClient(); err != nil {
			t.Fatalf("不配证书应通过：%v", err)
		}
	})

	t.Run("只配证书报错", func(t *testing.T) {
		cfg := validConfig()
		cfg.Server.TLSCert = cert
		err := cfg.ValidateClient()
		if err == nil || !strings.Contains(err.Error(), "必须同时配置") {
			t.Fatalf("应提示必须同时配置，得到 %v", err)
		}
	})

	t.Run("只配私钥报错", func(t *testing.T) {
		cfg := validConfig()
		cfg.Server.TLSKey = key
		if err := cfg.ValidateClient(); err == nil || !strings.Contains(err.Error(), "必须同时配置") {
			t.Fatalf("应提示必须同时配置，得到 %v", err)
		}
	})

	t.Run("证书对通过并保留路径", func(t *testing.T) {
		cfg := validConfig()
		cfg.Server.TLSCert, cfg.Server.TLSKey = cert, key
		if err := cfg.ValidateClient(); err != nil {
			t.Fatalf("合法证书对应通过：%v", err)
		}
		if cfg.Server.TLSCert != cert || cfg.Server.TLSKey != key {
			t.Fatalf("路径被意外改写：%q / %q", cfg.Server.TLSCert, cfg.Server.TLSKey)
		}
		if !cfg.Server.TLSEnabled() {
			t.Fatal("TLSEnabled 应为 true")
		}
	})

	t.Run("文件不存在报错", func(t *testing.T) {
		cfg := validConfig()
		cfg.Server.TLSCert = filepath.Join(t.TempDir(), "nope.pem")
		cfg.Server.TLSKey = key
		if err := cfg.ValidateClient(); err == nil || !strings.Contains(err.Error(), "无法加载") {
			t.Fatalf("应提示无法加载，得到 %v", err)
		}
	})

	t.Run("内容是垃圾报错", func(t *testing.T) {
		junk := writeTemp(t, "not a certificate")
		cfg := validConfig()
		cfg.Server.TLSCert, cfg.Server.TLSKey = junk, junk
		if err := cfg.ValidateClient(); err == nil || !strings.Contains(err.Error(), "无法加载") {
			t.Fatalf("应提示无法加载，得到 %v", err)
		}
	})
}
