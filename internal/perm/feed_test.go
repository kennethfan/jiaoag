package perm_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/policyfeed"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// installFeed 往 dir 里放一份已签名（或按需损坏）的 feed，返回公钥。
func installFeed(t *testing.T, dir, body string, tamper bool) policyfeed.Keys {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	data := []byte(body)
	sig := ed25519.Sign(private, data)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	stored := data
	if tamper {
		stored = []byte(strings.Replace(body, "feed.ls", "feed.evil", 1))
	}
	if err := os.WriteFile(filepath.Join(dir, policyfeed.FileName), stored, 0o644); err != nil {
		t.Fatalf("写 feed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, policyfeed.FileName+policyfeed.SignatureSuffix), []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		t.Fatalf("写签名: %v", err)
	}
	return policyfeed.Keys{public}
}

func feedBody(version string, issued time.Time, expires time.Time) string {
	s := "version: " + version + "\nissued_at: " + issued.Format(time.RFC3339) + "\n"
	if !expires.IsZero() {
		s += "expires_at: " + expires.Format(time.RFC3339) + "\n"
	}
	return s + "rules:\n  - id: feed.ls\n    programs: [ls]\n    reason: feed 要求审批\n"
}

func TestLoaderAppliesSignedFeedAsTightenRule(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	keys := installFeed(t, dir, feedBody("2026-11-24.1", now, now.Add(48*time.Hour)), false)

	rs, err := perm.NewLoader(dir).WithFeedKeys(keys).Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !strings.Contains(rs.Version, "+feed@2026-11-24.1") {
		t.Fatalf("版本应包含 feed 版本: %s", rs.Version)
	}
	d := rs.Evaluate(context.Background(), commandPrepared("ls", "-la"))
	if d.Verdict != perm.RequireUser || d.RuleID != "feed.ls" {
		t.Fatalf("feed 规则应把 ls 收紧为审批，实际 %+v", d)
	}
	// feed 只能收紧：同一条规则不能变成放行。
	if d.Verdict == perm.Allow {
		t.Fatal("feed 规则不允许放行")
	}
}

func TestLoaderRejectsBrokenSignature(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	keys := installFeed(t, dir, feedBody("v1", now, now.Add(time.Hour)), true)

	if _, err := perm.NewLoader(dir).WithFeedKeys(keys).Load(); err == nil {
		t.Fatal("签名不匹配时应报错")
	}
}

func TestLoaderRejectsExpiredFeed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	keys := installFeed(t, dir, feedBody("v1", now.Add(-48*time.Hour), now.Add(-time.Hour)), false)

	_, err := perm.NewLoader(dir).WithFeedKeys(keys).Load()
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期 feed 应报错，实际: %v", err)
	}
}

func TestLoaderRequiresKeyForFeedDirectory(t *testing.T) {
	if _, err := perm.NewLoader("").WithFeedKeys(policyfeed.Keys{make(ed25519.PublicKey, ed25519.PublicKeySize)}).Load(); err == nil {
		t.Fatal("配了公钥却没有目录时应报错")
	}
}

func TestUnsignedFeedFileActsAsLocalRule(t *testing.T) {
	// 不配公钥时 feed.yaml 只是目录里的一份本地规则——用户自己的目录本来就可信。
	dir := t.TempDir()
	body := "version: local\nrules:\n  - id: local.ls\n    programs: [ls]\n"
	if err := os.WriteFile(filepath.Join(dir, policyfeed.FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	rs, err := perm.NewLoader(dir).Load()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	d := rs.Evaluate(context.Background(), commandPrepared("ls"))
	if d.Verdict != perm.RequireUser || d.RuleID != "local.ls" {
		t.Fatalf("本地 feed.yaml 应作为收紧规则生效，实际 %+v", d)
	}
	_ = tools.EffectProcess
}

func TestFeedKeysBase64RoundTrip(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	keys, err := policyfeed.ParseKeys([]string{base64.StdEncoding.EncodeToString(public)})
	if err != nil {
		t.Fatalf("解析公钥: %v", err)
	}
	if len(keys) != 1 || !keys[0].Equal(public) {
		t.Fatal("公钥往返不一致")
	}
}
