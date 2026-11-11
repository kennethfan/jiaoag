// Package workspace 把工作区根目录包成一个受边界保护的文件访问入口。
//
// 这里是「无容器」前提下的第一道边界：agent 只能看到根目录内的普通文件，
// 绝对路径、.. 逃逸、指向根外的符号链接与 .git 内部一律拒绝。所有工具都
// 只通过本包触达磁盘，越界检查因此只有一处实现。
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrOutsideRoot 表示解析后的路径落在工作区之外。
	ErrOutsideRoot = errors.New("workspace: 路径越界")
	// ErrIgnored 表示路径命中不可访问名单（.git 等）。
	ErrIgnored = errors.New("workspace: 路径不可访问")
	// ErrBinary 表示文件含 NUL 字节，按二进制处理。
	ErrBinary = errors.New("workspace: 二进制文件")
	// ErrNotRegular 表示目标不是普通文件（目录、设备、链接等）。
	ErrNotRegular = errors.New("workspace: 不是普通文件")
)

// 读取上限：单次最多 1 MB、2000 行。工具层不再各自判断，避免三处漂移。
const (
	MaxReadBytes = 1 << 20
	MaxReadLines = 2000

	binaryProbeBytes = 8 << 10
)

// DefaultIgnoreDirs 是遍历时跳过的目录名。
//
// 不解析 .gitignore：v1 用固定名单加结果上界兜底，噪声真的影响可用性时再补。
var DefaultIgnoreDirs = map[string]bool{
	".git":         true,
	".jiaoag":      true,
	"node_modules": true,
	"vendor":       true,
}

// Workspace 是工作区根的句柄。零值不可用，用 New 构造。
type Workspace struct {
	root string
}

// New 解析并锁定工作区根目录。符号链接在构造时解析一次，之后不再变化。
func New(root string) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace: 根目录为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: 解析根目录 %q: %w", root, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: 解析根目录符号链接 %q: %w", abs, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("workspace: 读取根目录 %q: %w", real, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: %q 不是目录", real)
	}
	return &Workspace{root: real}, nil
}

// Root 返回解析后的绝对路径。
func (w *Workspace) Root() string { return w.root }

// Resolve 把相对路径解析成绝对路径，并校验它没有越出根目录。
//
// 对最深的已存在祖先求真实路径，因此目标文件尚不存在也能解析；指向根外的
// 符号链接在这一步被拒绝。
func (w *Workspace) Resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w：不接受绝对路径 %q", ErrOutsideRoot, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w：%q", ErrOutsideRoot, rel)
	}
	if hasComponent(clean, ".git") {
		return "", fmt.Errorf("%w：%q 位于 .git 内", ErrIgnored, rel)
	}

	abs := filepath.Join(w.root, clean)
	real, err := evalExistingPrefix(abs)
	if err != nil {
		return "", err
	}
	if real != w.root && !strings.HasPrefix(real, w.root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w：%q 解析后落在工作区外", ErrOutsideRoot, rel)
	}
	return abs, nil
}

// FileContent 是一次读取的结果。
type FileContent struct {
	Path       string
	Lines      []string
	TotalLines int
	Truncated  bool
}

// Read 读取工作区内的文本文件。
//
// offset 是 1 起始的行号，limit <= 0 时用 MaxReadLines。整文件超过 1 MB 时
// 只读前 1 MB 并在结果里标记 Truncated。
func (w *Workspace) Read(rel string, offset, limit int) (FileContent, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		return FileContent{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return FileContent{}, fmt.Errorf("workspace: 读取 %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return FileContent{}, fmt.Errorf("%w：%q", ErrNotRegular, rel)
	}

	f, err := os.Open(abs)
	if err != nil {
		return FileContent{}, fmt.Errorf("workspace: 打开 %q: %w", rel, err)
	}
	defer f.Close()

	probe := make([]byte, binaryProbeBytes)
	n, err := f.Read(probe)
	if err != nil && n == 0 {
		return FileContent{}, fmt.Errorf("workspace: 读取 %q: %w", rel, err)
	}
	if containsNUL(probe[:n]) {
		return FileContent{}, fmt.Errorf("%w：%q", ErrBinary, rel)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return FileContent{}, fmt.Errorf("workspace: 定位 %q: %w", rel, err)
	}

	buf := make([]byte, MaxReadBytes)
	m, err := f.Read(buf)
	if err != nil && m == 0 {
		return FileContent{}, fmt.Errorf("workspace: 读取 %q: %w", rel, err)
	}
	byteTruncated := info.Size() > MaxReadBytes

	text := string(buf[:m])
	all := strings.Split(text, "\n")
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	total := len(all)
	if byteTruncated && m > 0 && text[len(text)-1] != '\n' {
		// 最后一行可能被字节截断切掉一半，直接丢弃，避免展示残缺内容。
		total--
	}

	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 || limit > MaxReadLines {
		limit = MaxReadLines
	}
	if offset > total {
		return FileContent{Path: rel, TotalLines: total, Truncated: byteTruncated}, nil
	}
	end := offset - 1 + limit
	// 上界用 total 而不是 len(all)：字节截断时 all 的最后一元素是残行，
	// 它已被 total-- 排除在外，这里再放进来就会输出半行并让行号错位。
	if end > total {
		end = total
	}
	lines := make([]string, end-(offset-1))
	copy(lines, all[offset-1:end])

	return FileContent{
		Path:       rel,
		Lines:      lines,
		TotalLines: total,
		Truncated:  byteTruncated || end < total,
	}, nil
}

// Walk 遍历工作区内的普通文件，回调收到相对路径与字节数（正斜杠分隔）。
//
// 默认忽略 DefaultIgnoreDirs 下的目录；不跟随符号链接。
func (w *Workspace) Walk(rel string, fn func(rel string, size int64) error) error {
	abs, err := w.Resolve(rel)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != abs && DefaultIgnoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(relPath), info.Size())
	})
}

func evalExistingPrefix(abs string) (string, error) {
	rest := ""
	current := abs
	for {
		real, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("workspace: 解析符号链接 %q: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("workspace: 无法解析 %q", abs)
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

func hasComponent(path, name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == name {
			return true
		}
	}
	return false
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
