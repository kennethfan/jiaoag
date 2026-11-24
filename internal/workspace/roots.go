package workspace

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ErrUnknownRoot 表示路径里写了别名，但会话没有挂这个根。
var ErrUnknownRoot = errors.New("workspace: 未知的根别名")

// aliasPattern 限制别名形态：它以冒号分隔路径，因此不能含冒号、空白，
// 也不能用大写开头（避免和 Windows 盘符 C: 混淆）。
var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ValidAlias 判断别名是否合法。
func ValidAlias(alias string) bool { return aliasPattern.MatchString(alias) }

// NamedRoot 是会话挂载的一个附加根。
type NamedRoot struct {
	Alias string
	Path  string
}

// Roots 是会话可见的根集合：主根可写，附加根只读（ADR-0036）。
//
// 只读的边界在工具层：写入类工具只接受主根，命令的 cwd 永远是主根。
// Roots 自己不做权限判断，它只回答「这个名字对应哪个目录」。
type Roots struct {
	primary *Workspace
	byAlias map[string]*Workspace
	aliases []string
}

// NewRoots 构造根集合。primary 必需；extras 里的别名必须合法且不重复，
// 路径必须存在且是目录，也不能与主根或彼此重复。
func NewRoots(primary *Workspace, extras []NamedRoot) (*Roots, error) {
	if primary == nil {
		return nil, errors.New("workspace: 主根为空")
	}
	r := &Roots{primary: primary, byAlias: make(map[string]*Workspace, len(extras))}
	seenPath := map[string]string{primary.root: ""}
	for _, extra := range extras {
		alias := strings.TrimSpace(extra.Alias)
		if !ValidAlias(alias) {
			return nil, fmt.Errorf("workspace: 根别名 %q 不合法：只允许小写字母开头、字母数字与 _-，最多 32 字符", extra.Alias)
		}
		if _, dup := r.byAlias[alias]; dup {
			return nil, fmt.Errorf("workspace: 根别名 %q 重复", alias)
		}
		ws, err := New(extra.Path)
		if err != nil {
			return nil, fmt.Errorf("workspace: 附加根 %q: %w", alias, err)
		}
		if other, dup := seenPath[ws.root]; dup {
			if other == "" {
				return nil, fmt.Errorf("workspace: 附加根 %q 与主根是同一个目录", alias)
			}
			return nil, fmt.Errorf("workspace: 附加根 %q 与 %q 指向同一个目录", alias, other)
		}
		seenPath[ws.root] = alias
		r.byAlias[alias] = ws
		r.aliases = append(r.aliases, alias)
	}
	sort.Strings(r.aliases)
	return r, nil
}

// Primary 返回主根（可写）。
func (r *Roots) Primary() *Workspace { return r.primary }

// Extra 返回附加根的规范路径，供持久化使用（符号链接已解析）。
func (r *Roots) Extra() []SessionRootView {
	if r == nil {
		return nil
	}
	out := make([]SessionRootView, 0, len(r.aliases))
	for _, alias := range r.aliases {
		out = append(out, SessionRootView{Alias: alias, Path: r.byAlias[alias].root})
	}
	return out
}

// SessionRootView 是 Extra 的返回元素，避免 workspace 依赖 domain。
type SessionRootView struct {
	Alias string
	Path  string
}

// Aliases 返回附加根别名（已排序）。
func (r *Roots) Aliases() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.aliases...)
}

// Get 按别名查附加根。
func (r *Roots) Get(alias string) (*Workspace, bool) {
	if r == nil {
		return nil, false
	}
	ws, ok := r.byAlias[alias]
	return ws, ok
}

// Resolve 解析「alias:path」或普通相对路径。
//
// 不带已知别名前缀时一律落在主根；写了形如别名的前缀但没挂载时报错，
// 而不是悄悄当成主根下的文件名——拼错别名应该被发现。
func (r *Roots) Resolve(spec string) (ws *Workspace, rel, alias string, err error) {
	spec = strings.TrimSpace(spec)
	if r == nil {
		return nil, "", "", errors.New("workspace: 根集合为空")
	}
	if i := strings.Index(spec, ":"); i > 0 && ValidAlias(spec[:i]) {
		name := spec[:i]
		target, ok := r.byAlias[name]
		if !ok {
			return nil, "", "", fmt.Errorf("%w：%q", ErrUnknownRoot, name)
		}
		rest := spec[i+1:]
		if strings.TrimSpace(rest) == "" {
			rest = "."
		}
		return target, rest, name, nil
	}
	return r.primary, spec, "", nil
}

// Join 把绝对的根内路径还原成展示用的「alias:相对路径」。
func (r *Roots) Join(abs string) string {
	if r == nil {
		return abs
	}
	clean := filepath.Clean(abs)
	if rel, ok := relativeTo(r.primary.root, clean); ok {
		return rel
	}
	for _, alias := range r.aliases {
		if rel, ok := relativeTo(r.byAlias[alias].root, clean); ok {
			return alias + ":" + rel
		}
	}
	return abs
}

func relativeTo(root, path string) (string, bool) {
	if path == root {
		return ".", true
	}
	prefix := root + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return filepath.ToSlash(strings.TrimPrefix(path, prefix)), true
}
