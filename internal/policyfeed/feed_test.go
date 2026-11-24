package policyfeed_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethfan/jiaoag/internal/policyfeed"
)

// signedFeed 生成一份带签名的 feed，返回内容、签名与公钥（base64）。
func signedFeed(t *testing.T, body string) (data, sig []byte, pub string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	data = []byte(body)
	sig = ed25519.Sign(private, data)
	return data, sig, base64.StdEncoding.EncodeToString(public)
}

func feedYAML(version string, issued, expires time.Time) string {
	s := "version: " + version + "\n"
	s += "issued_at: " + issued.Format(time.RFC3339) + "\n"
	if !expires.IsZero() {
		s += "expires_at: " + expires.Format(time.RFC3339) + "\n"
	}
	s += "rules:\n  - id: feed.ls\n    programs: [ls]\n    reason: feed 要求审批\n"
	return s
}

func writeSource(t *testing.T, data, sig []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "feed.yaml"), data, 0o644); err != nil {
		t.Fatalf("写 feed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feed.yaml.sig"), []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		t.Fatalf("写签名: %v", err)
	}
	return "file://" + filepath.Join(dir, "feed.yaml")
}

func TestSyncInstallsVerifiedFeed(t *testing.T) {
	now := time.Now()
	data, sig, pub := signedFeed(t, feedYAML("v1", now, now.Add(24*time.Hour)))
	url := writeSource(t, data, sig)
	keys, err := policyfeed.ParseKeys([]string{pub})
	if err != nil {
		t.Fatalf("解析公钥: %v", err)
	}

	dir := t.TempDir()
	res, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: url, Keys: keys, Dir: dir})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if !res.Changed || res.Version != "v1" {
		t.Fatalf("结果不对: %+v", res)
	}
	installed, meta, err := policyfeed.LoadFile(dir, keys)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if string(installed) != string(data) || meta.Version != "v1" {
		t.Fatalf("安装内容不对: %q %+v", installed, meta)
	}
	state, err := policyfeed.ReadState(filepath.Join(dir, policyfeed.StateName))
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if state.Version != "v1" || state.SHA256 != res.SHA256 {
		t.Fatalf("状态不对: %+v", state)
	}
}

func TestSyncRejectsTamperedFeed(t *testing.T) {
	now := time.Now()
	data, sig, pub := signedFeed(t, feedYAML("v1", now, now.Add(time.Hour)))
	url := writeSource(t, []byte(strings.Replace(string(data), "feed.ls", "feed.other", 1)), sig)
	keys, _ := policyfeed.ParseKeys([]string{pub})

	dir := t.TempDir()
	if _, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: url, Keys: keys, Dir: dir}); err == nil {
		t.Fatal("内容被改后应该验签失败")
	}
	if _, err := os.Stat(filepath.Join(dir, policyfeed.FileName)); !os.IsNotExist(err) {
		t.Fatal("验签失败不应落盘")
	}
}

func TestSyncIsIdempotentAndBlocksRollback(t *testing.T) {
	now := time.Now()
	data, sig, pub := signedFeed(t, feedYAML("v2", now, now.Add(time.Hour)))
	keys, _ := policyfeed.ParseKeys([]string{pub})
	dir := t.TempDir()

	if _, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: writeSource(t, data, sig), Keys: keys, Dir: dir}); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	res, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: writeSource(t, data, sig), Keys: keys, Dir: dir})
	if err != nil {
		t.Fatalf("重复同步: %v", err)
	}
	if res.Changed {
		t.Fatal("内容没变时不该标记为已更新")
	}

	// 更旧签发时间的 feed 必须被拒。
	old, oldSig, oldPub := signedFeed(t, feedYAML("v1", now.Add(-2*time.Hour), now.Add(time.Hour)))
	oldKeys, _ := policyfeed.ParseKeys([]string{oldPub})
	_, err = policyfeed.Sync(context.Background(), policyfeed.Options{URL: writeSource(t, old, oldSig), Keys: oldKeys, Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("应该拒绝回滚，实际: %v", err)
	}
}

func TestSyncRejectsExpiredFeedUnlessForced(t *testing.T) {
	now := time.Now()
	data, sig, pub := signedFeed(t, feedYAML("v1", now.Add(-48*time.Hour), now.Add(-time.Hour)))
	keys, _ := policyfeed.ParseKeys([]string{pub})
	dir := t.TempDir()
	url := writeSource(t, data, sig)

	if _, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: url, Keys: keys, Dir: dir}); err == nil {
		t.Fatal("过期 feed 应被拒绝")
	}
	if _, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: url, Keys: keys, Dir: dir, Force: true}); err != nil {
		t.Fatalf("强制安装应成功: %v", err)
	}
}

func TestSyncRejectsMissingKeyAndBadURL(t *testing.T) {
	keys, _ := policyfeed.ParseKeys([]string{base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))})
	if _, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: "file:///nope", Keys: nil, Dir: t.TempDir()}); err == nil {
		t.Fatal("没有公钥时应报错")
	}
	if _, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: "", Keys: keys, Dir: t.TempDir()}); err == nil {
		t.Fatal("没有地址时应报错")
	}
}

func TestSyncOverHTTP(t *testing.T) {
	now := time.Now()
	data, sig, pub := signedFeed(t, feedYAML("v1", now, now.Add(time.Hour)))
	keys, _ := policyfeed.ParseKeys([]string{pub})
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.yaml", func(w http.ResponseWriter, r *http.Request) { w.Write(data) })
	mux.HandleFunc("/feed.yaml.sig", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(base64.StdEncoding.EncodeToString(sig)))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	dir := t.TempDir()
	res, err := policyfeed.Sync(context.Background(), policyfeed.Options{URL: ts.URL + "/feed.yaml", Keys: keys, Dir: dir})
	if err != nil {
		t.Fatalf("http 同步失败: %v", err)
	}
	if res.Version != "v1" {
		t.Fatalf("版本不对: %+v", res)
	}
}

func TestParseKeysRejectsBadInput(t *testing.T) {
	cases := [][]string{
		nil,
		{""},
		{"not-base64!!"},
		{base64.StdEncoding.EncodeToString([]byte("short"))},
	}
	for _, in := range cases {
		if _, err := policyfeed.ParseKeys(in); err == nil {
			t.Fatalf("%v 应报错", in)
		}
	}
}

func TestParseRequiresVersionAndIssuedAt(t *testing.T) {
	if _, err := policyfeed.Parse([]byte("rules: []\n")); err == nil {
		t.Fatal("缺 version 应报错")
	}
	if _, err := policyfeed.Parse([]byte("version: v1\nrules: []\n")); err == nil {
		t.Fatal("缺 issued_at 应报错")
	}
}
