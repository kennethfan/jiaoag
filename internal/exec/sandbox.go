package exec

import (
	"os"
	"path/filepath"
	"strings"
)

// sandboxPaths 是隔离后端需要知道的两组路径：可写根与禁止读取的敏感目录。
//
// 路径来源只用命令自己的 Env 与 Command.Cwd，不读服务端进程环境：命令工具的
// Env 已经过白名单清洗，沙箱规则必须与它看到的 HOME/TMPDIR 一致，否则会出现
// 「规则按服务端的 HOME 生成、命令按另一个 HOME 跑」的错位。
type sandboxPaths struct {
	// writable 是唯一允许写入的根（工作区、临时目录、工具链缓存）。
	writable []string
	// denied 是禁止读取的敏感目录（密钥、云凭据、配置）。
	denied []string
}

// sandboxPolicyPaths 计算本次命令的隔离路径集合。
func sandboxPolicyPaths(cmd Command) sandboxPaths {
	var out sandboxPaths
	seen := make(map[string]bool)

	add := func(p string) {
		for _, c := range canonicalPaths(p) {
			if !seen[c] {
				seen[c] = true
				out.writable = append(out.writable, c)
			}
		}
	}

	add(cmd.Cwd)
	for _, key := range []string{"TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR"} {
		add(envValue(cmd.Env, key))
	}
	add(os.TempDir())
	for _, p := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		add(p)
	}

	home := envValue(cmd.Env, "HOME")
	if home != "" {
		for _, rel := range []string{"Library/Caches", "go", ".cache", ".npm"} {
			add(filepath.Join(home, rel))
		}
		cwdPaths := canonicalPaths(cmd.Cwd)
		for _, rel := range []string{
			".ssh", ".aws", ".gnupg", ".config", ".kube", ".docker",
			".netrc", ".npmrc", ".git-credentials", ".password-store",
			"Library/Keychains",
		} {
			for _, p := range canonicalPaths(filepath.Join(home, rel)) {
				// 工作区恰好落在敏感目录下时，宁可放弃这一条 deny，也不能让沙箱
				// 把自己的工作区判定成禁区。
				if anyContains(p, cwdPaths) {
					continue
				}
				out.denied = append(out.denied, p)
			}
		}
	}
	return out
}

// anyContains 报告 paths 中是否有路径落在 parent 之下。
func anyContains(parent string, paths []string) bool {
	for _, p := range paths {
		if pathContains(parent, p) {
			return true
		}
	}
	return false
}

// canonicalPaths 返回路径本身与解析符号链接后的真实路径。
//
// macOS 上 /tmp、/var 都是 /private/* 的符号链接，而 seatbelt 的 subpath
// 匹配的是真实路径：只写符号链接形态会让规则静默失效。
func canonicalPaths(p string) []string {
	if p == "" {
		return nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil
	}
	abs = filepath.Clean(abs)
	out := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		out = append(out, resolved)
	}
	return out
}

// envValue 从 K=V 列表里取第一个匹配项；命令 Env 已经去重。
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// pathContains 报告 child 是否等于 parent 或位于其下。
func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
