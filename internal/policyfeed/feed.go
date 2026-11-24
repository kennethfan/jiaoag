// Package policyfeed 处理外部的签名规则 feed。
//
// 规则 feed 解决的是「本机不知道的新威胁」：包里昨天有没有被投毒、某个程序的
// 哪个 flag 能绕过审批，这些推不出来，只能靠外部持续更新的数据。它是**只收紧**
// 的数据（perm.Loader 强制 Tighten），所以即使来源不可信，最坏结果也只是多要
// 几次审批。
//
// 文件形态刻意做得很土：feed.yaml 是普通规则文件，签名放在旁边的 feed.yaml.sig
// （ed25519，64 字节 base64）。好处是 diff 可读、可以手工核，任何编辑器都能看。
//
// 这个包只负责「取、验、落盘」，不解析规则语义——那是 internal/perm 的事。
package policyfeed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// FileName 是 feed 落在 policy.d 里的文件名。
	FileName = "feed.yaml"

	// SignatureSuffix 是分离签名的后缀。
	SignatureSuffix = ".sig"

	// StateName 记录上次成功同步的版本与摘要，用来挡回滚。
	StateName = "feed.state"

	// MaxBytes 限制单次下载大小。规则文件是数据，不该大到把内存吃满。
	MaxBytes = 1 << 20
)

// Metadata 是 feed 文件自己的头信息。它也在签名覆盖范围内，所以版本与有效期
// 不能被中间人改写。
type Metadata struct {
	Version   string    `yaml:"version"`
	IssuedAt  time.Time `yaml:"issued_at"`
	ExpiresAt time.Time `yaml:"expires_at"`
}

// State 记录上一次成功同步的结果，只用于回滚检测。
type State struct {
	Version  string    `yaml:"version"`
	IssuedAt time.Time `yaml:"issued_at"`
	SHA256   string    `yaml:"sha256"`
	SyncedAt time.Time `yaml:"synced_at"`
}

// Keys 是一组可接受的 ed25519 公钥。多把是为了轮换：新旧公钥同时在线，
// 换完再把旧的删掉。
type Keys []ed25519.PublicKey

// ParseKeys 解析 base64 编码的公钥（标准与 URL 两种字母表都接受）。
func ParseKeys(values []string) (Keys, error) {
	if len(values) == 0 {
		return nil, errors.New("policyfeed: 没有配置公钥")
	}
	keys := make(Keys, 0, len(values))
	for i, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			return nil, fmt.Errorf("policyfeed: 第 %d 个公钥为空", i+1)
		}
		bin, err := decodeBase64(v)
		if err != nil {
			return nil, fmt.Errorf("policyfeed: 第 %d 个公钥不是合法 base64: %w", i+1, err)
		}
		if len(bin) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("policyfeed: 第 %d 个公钥长度 %d，应为 %d", i+1, len(bin), ed25519.PublicKeySize)
		}
		keys = append(keys, ed25519.PublicKey(bin))
	}
	return keys, nil
}

// Verify 检查签名是否由任意一把已知公钥产生。
func (k Keys) Verify(data, sig []byte) error {
	if len(k) == 0 {
		return errors.New("policyfeed: 没有可用的公钥")
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("policyfeed: 签名长度 %d，应为 %d", len(sig), ed25519.SignatureSize)
	}
	for _, key := range k {
		if ed25519.Verify(key, data, sig) {
			return nil
		}
	}
	return errors.New("policyfeed: 签名不匹配任何已知公钥")
}

// Parse 解析 feed 头信息。缺 version 或 issued_at 直接报错：没有这两项就无法
// 做回滚判断。
func Parse(data []byte) (Metadata, error) {
	var meta Metadata
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return Metadata{}, fmt.Errorf("policyfeed: 解析元信息: %w", err)
	}
	meta.Version = strings.TrimSpace(meta.Version)
	if meta.Version == "" {
		return Metadata{}, errors.New("policyfeed: feed 缺少 version")
	}
	if meta.IssuedAt.IsZero() {
		return Metadata{}, errors.New("policyfeed: feed 缺少 issued_at")
	}
	return meta, nil
}

// Digest 返回内容的 sha256（十六进制）。
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Options 是一次同步的参数。
type Options struct {
	URL     string
	Keys    Keys
	Dir     string
	Client  *http.Client
	Now     func() time.Time
	Force   bool
	MaxSize int64
}

// Result 描述同步结果。
type Result struct {
	Path      string
	Version   string
	IssuedAt  time.Time
	ExpiresAt time.Time
	SHA256    string
	Changed   bool
}

// Sync 下载、验签、挡回滚，然后把 feed 与签名原子写入目录。
//
// 任何一步失败都不会改动现有文件：先全部校验完，最后才 rename。
func Sync(ctx context.Context, opts Options) (Result, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return Result{}, errors.New("policyfeed: 缺少 feed 地址")
	}
	if len(opts.Keys) == 0 {
		return Result{}, errors.New("policyfeed: 缺少公钥，无法验签")
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return Result{}, errors.New("policyfeed: 缺少目标目录")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	limit := opts.MaxSize
	if limit <= 0 {
		limit = MaxBytes
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	data, err := fetch(ctx, client, opts.URL, limit)
	if err != nil {
		return Result{}, err
	}
	sigRaw, err := fetch(ctx, client, opts.URL+SignatureSuffix, limit)
	if err != nil {
		return Result{}, err
	}
	sig, err := decodeBase64(strings.TrimSpace(string(sigRaw)))
	if err != nil {
		return Result{}, fmt.Errorf("policyfeed: 解码签名: %w", err)
	}
	if err := opts.Keys.Verify(data, sig); err != nil {
		return Result{}, err
	}
	meta, err := Parse(data)
	if err != nil {
		return Result{}, err
	}
	if !meta.ExpiresAt.IsZero() && now().After(meta.ExpiresAt) && !opts.Force {
		return Result{}, fmt.Errorf("policyfeed: feed 已于 %s 过期（用 -force 可强制安装）",
			meta.ExpiresAt.Format(time.RFC3339))
	}

	dir := opts.Dir
	path := filepath.Join(dir, FileName)
	digest := Digest(data)

	state, err := ReadState(filepath.Join(dir, StateName))
	if err != nil {
		return Result{}, err
	}
	changed := state.SHA256 != digest
	if !changed {
		return Result{
			Path: path, Version: meta.Version, IssuedAt: meta.IssuedAt,
			ExpiresAt: meta.ExpiresAt, SHA256: digest, Changed: false,
		}, nil
	}
	// 回滚检测：新 feed 的签发时间必须晚于已安装的那份。只比版本字符串不可靠
	// （版本命名是发布方的自由），签发时间才是可比较的顺序。
	if !opts.Force && !state.IssuedAt.IsZero() && !meta.IssuedAt.After(state.IssuedAt) {
		return Result{}, fmt.Errorf("policyfeed: 拒绝回滚：已安装 %s（%s），收到 %s（%s）",
			state.Version, state.IssuedAt.Format(time.RFC3339),
			meta.Version, meta.IssuedAt.Format(time.RFC3339))
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("policyfeed: 创建目录 %s: %w", dir, err)
	}
	if err := writeAtomic(path, data); err != nil {
		return Result{}, err
	}
	if err := writeAtomic(path+SignatureSuffix, []byte(base64.StdEncoding.EncodeToString(sig))); err != nil {
		return Result{}, err
	}
	next := State{Version: meta.Version, IssuedAt: meta.IssuedAt, SHA256: digest, SyncedAt: now()}
	if err := writeState(filepath.Join(dir, StateName), next); err != nil {
		return Result{}, err
	}
	return Result{
		Path: path, Version: meta.Version, IssuedAt: meta.IssuedAt,
		ExpiresAt: meta.ExpiresAt, SHA256: digest, Changed: true,
	}, nil
}

// ReadState 读取同步状态。文件不存在返回零值：第一次同步是正常状态。
func ReadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("policyfeed: 读取同步状态 %s: %w", path, err)
	}
	var st State
	if err := yaml.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("policyfeed: 解析同步状态 %s: %w", path, err)
	}
	return st, nil
}

// LoadFile 读取并校验目录里已安装的 feed，供服务端启动时使用。
//
// 返回 meta 与内容；签名不对、文件缺失都算错误。
func LoadFile(dir string, keys Keys) ([]byte, Metadata, error) {
	if len(keys) == 0 {
		return nil, Metadata{}, errors.New("policyfeed: 没有可用的公钥")
	}
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("policyfeed: 读取 %s: %w", path, err)
	}
	sigRaw, err := os.ReadFile(path + SignatureSuffix)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("policyfeed: 读取签名 %s: %w", path+SignatureSuffix, err)
	}
	sig, err := decodeBase64(strings.TrimSpace(string(sigRaw)))
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("policyfeed: 解码签名: %w", err)
	}
	if err := keys.Verify(data, sig); err != nil {
		return nil, Metadata{}, err
	}
	meta, err := Parse(data)
	if err != nil {
		return nil, Metadata{}, err
	}
	return data, meta, nil
}

// fetch 支持 http(s) 与 file:// 两种来源。file:// 是为了离线分发与测试：
// 内网共享目录、CI 产物都能直接当 feed 源。
func fetch(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	if path, ok := filePath(rawURL); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("policyfeed: 读取 %s: %w", rawURL, err)
		}
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("policyfeed: %s 超过 %d 字节", rawURL, limit)
		}
		return b, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("policyfeed: 构造请求: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("policyfeed: 下载 %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("policyfeed: 下载 %s: HTTP %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("policyfeed: 读取响应: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("policyfeed: %s 超过 %d 字节", rawURL, limit)
	}
	return body, nil
}

func filePath(rawURL string) (string, bool) {
	const prefix = "file://"
	if !strings.HasPrefix(rawURL, prefix) {
		return "", false
	}
	return strings.TrimPrefix(rawURL, prefix), true
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".feed-*")
	if err != nil {
		return fmt.Errorf("policyfeed: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("policyfeed: 写入 %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("policyfeed: 关闭 %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("policyfeed: 设置权限 %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("policyfeed: 替换 %s: %w", path, err)
	}
	return nil
}

func writeState(path string, st State) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(st); err != nil {
		return fmt.Errorf("policyfeed: 编码同步状态: %w", err)
	}
	enc.Close()
	return writeAtomic(path, buf.Bytes())
}

func decodeBase64(s string) ([]byte, error) {
	if bin, err := base64.StdEncoding.DecodeString(s); err == nil {
		return bin, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
