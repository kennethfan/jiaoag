// Package tools 定义工具契约与只读工具实现。
//
// 两段式（Prepare / Execute）在 M3 看起来多余——读工具没有副作用可分——
// 但审批要插在两者之间（ADR-0006），契约先定型，M4 只增加一种副作用类别，
// 循环与调用方都不用改。
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Effect 是工具可能产生的副作用类别。只读工具只有 read。
type Effect string

const (
	EffectRead         Effect = "read"
	EffectProposeWrite Effect = "propose_write"
	EffectProcess      Effect = "process"
	EffectNetwork      Effect = "network"
)

// Spec 是工具对模型的自描述。
type Spec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema，直接转给供应商
	Effects     []Effect
	Version     string // 进 extensions_digest：schema 变了就要变
}

// Call 是一次工具调用的原始输入。
type Call struct {
	ID   string
	Name string
	Args json.RawMessage
}

// Prepared 是 Prepare 的产物：参数已校验、路径已解析。
//
// Args 存放工具自己的参数结构体，Execute 里做类型断言；Paths 用于日志与
// 权限判定，是绝对路径。Change 只有写入工具会填，表示「一次还没发生的写入」。
type Prepared struct {
	Call  Call
	Spec  Spec
	Args  any
	Paths []string
	// Change 非空时，这次调用必须先经过用户审批才能落盘；此时顶层不会调用
	// Execute，而是由 agent 走「提案 → 审批 → CAS 落盘」。
	Change *Change
}

// Change 是一次「将要发生的写入」。
//
// 它在 Prepare 阶段就算好（旧内容、旧摘要、新内容、diff），审批通过后才由
// agent 调 workspace.Write 落盘。写入工具的 Execute 不碰磁盘，因此「谁写的」
// 只有一条路径，CAS 也只需要在一处实现。
type Change struct {
	Path     string // 工作区相对路径
	Exists   bool   // 原文件是否存在（决定 diff 头是 /dev/null 还是 a/<path>）
	BaseHash string // 原文件 sha256；新建文件为空串
	NewHash  string // 新内容 sha256
	// NewContent 是最终要写进磁盘的字节（换行风格已按原文件还原）。
	NewContent []byte
	Diff       string
	// Truncated 表示 diff 超过 patch.MaxDiffBytes 被截断：审批面板要提示用户
	// 「你看到的不是全部」，不能让人盲批一份没看全的改动。
	Truncated bool
}

// Result 是工具执行结果。IsError 表示「这次调用失败」，不是「任务失败」：
// 结果会作为 tool_result 回给模型，由它决定换目标还是放弃。
type Result struct {
	Content string
	IsError bool
	// Truncated 表示输出被 MaxOutputBytes 截过：事件载荷会带上它，
	// 界面与回放据此知道「这段不是全部」。
	Truncated bool
	Elapsed   time.Duration
}

// MaxOutputBytes 是单次工具输出的硬上限。工具的参数上界（max_results 等）
// 约束条数，但一条 grep 匹配行本身可达 1 MB，没有字节上限时单个
// tool.result 事件就能把事件日志和界面撑爆。
const MaxOutputBytes = 256 << 10

// Tool 是一个可被模型调用的工具。
type Tool interface {
	Spec() Spec
	Prepare(ctx context.Context, call Call) (Prepared, error)
	Execute(ctx context.Context, p Prepared) (Result, error)
}

// ErrInvalidArgs 表示模型给的参数不合法。它属于工具错误，不是任务错误。
type ErrInvalidArgs struct {
	Tool    string
	Message string
}

func (e *ErrInvalidArgs) Error() string {
	return fmt.Sprintf("tools: %s 参数无效：%s", e.Tool, e.Message)
}

func invalidArgs(tool, format string, a ...any) error {
	return &ErrInvalidArgs{Tool: tool, Message: fmt.Sprintf(format, a...)}
}

// decodeArgs 把参数 JSON 解进 v；空参数按 {} 处理。
func decodeArgs(call Call, v any) error {
	raw := strings.TrimSpace(string(call.Args))
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return invalidArgs(call.Name, "不是合法的 JSON 对象：%v", err)
	}
	return nil
}

// Registry 是按名字索引的工具集。
type Registry struct {
	byName map[string]Tool
	names  []string
}

// NewRegistry 构造注册表；重名直接报错，避免「谁的 schema 生效」这种隐性问题。
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		spec := t.Spec()
		if spec.Name == "" {
			return nil, fmt.Errorf("tools: 工具名为空")
		}
		if _, ok := r.byName[spec.Name]; ok {
			return nil, fmt.Errorf("tools: 工具 %q 重复注册", spec.Name)
		}
		r.byName[spec.Name] = t
		r.names = append(r.names, spec.Name)
	}
	sort.Strings(r.names)
	return r, nil
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Names 返回排序后的工具名。
func (r *Registry) Names() []string {
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// Specs 返回按名字排序的工具描述，保证提示词与 digest 稳定。
func (r *Registry) Specs() []Spec {
	out := make([]Spec, 0, len(r.names))
	for _, name := range r.names {
		out = append(out, r.byName[name].Spec())
	}
	return out
}

// Digest 是工具集指纹：名字 + 版本。schema 改动必须同时改 Version。
func (r *Registry) Digest() string {
	var b strings.Builder
	for _, name := range r.names {
		spec := r.byName[name].Spec()
		fmt.Fprintf(&b, "%s@%s;", spec.Name, spec.Version)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
