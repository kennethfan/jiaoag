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

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/limits"
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
	// Source 是工具来源（builtin | mcp:foo | plugin:bar），审计与审批面板靠它
	// 定位「这是谁提供的工具」。注册表聚合时由 Source 填充，工具自己不必关心。
	Source  string
	Version string // 进 extensions_digest：schema 变了就要变
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
	// Command 非空表示这是一次命令执行。工具只填 argv/cwd/env，执行后端由
	// 宿主注入（ADR-0011），工具无权指定。
	Command *Command
	// External 非空表示工具在宿主进程之外执行（MCP / 插件）：宿主既拿不到
	// diff，也拿不到 argv，只能在调用前后做审批与审计。它是 ADR-0001 信任
	// 模型的一部分——外部工具是受信代码，不是沙箱。
	External *External
	// LogPath 由 agent 填入：它知道 session/task/tool_call 三元组，工具不需要
	// 知道会话标识。为空表示不落全文日志。
	LogPath string
}

// Change 是一次「将要发生的写入」。
// 它在 Prepare 阶段就算好（旧内容、旧摘要、新内容、diff），审批通过后才由
// agent 调 workspace.Write 落盘。写入工具的 Execute 不碰磁盘，因此「谁写的」
// 只有一条路径，CAS 也只需要在一处实现。
type Change struct {
	Path string // 展示用路径：主根下是相对路径，附加根下是 alias:相对路径
	// Root 是附加根别名（空表示主根）。落盘与 CAS 需要它来选对工作区，
	// Path 只负责给人看（ADR-0037）。
	Root string
	// Rel 是在 Root 内的相对路径；主根下与 Path 相同。
	Rel      string
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

// External 描述一次由宿主之外执行的工具调用。
//
// 与 Change / Command 的区别：宿主无法在准备阶段算清后果，也无法代劳写盘；
// 能提供的只有「来源可追溯 + 审批门控 + 结果审计」。因此它不参与
// propose_write 的中介路径，永远走独立的审批分支。
type External struct {
	// Source 是工具来源，如 mcp:filesystem。审计与审批面板都靠它定位。
	Source string
	// Trusted 表示本地配置显式放行了这个来源；只有配置能给出，工具不能自称。
	Trusted bool
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

	// 以下字段只对命令工具有值。ExitCode 为 nil 表示没跑到「有退出码」这一步
	// （后端不可用、参数错误等）。
	ExitCode   *int
	OutputPath string
	// BackendVersion 是执行后端自报的身份（如 direct/darwin），随审计落库：
	// 事后要能回答「这条命令是在哪个后端上跑出来的」。
	BackendVersion string
	TimedOut       bool
	// LogError 非空表示全文日志没有完整落盘，工具结果里必须带上警告。
	LogError string
	// Limits 是这次执行实际套用的资源上限，零值表示没有配置。
	Limits execpkg.Limits
	// LimitsDegraded 表示请求了限制但当前平台做不到，审计里要如实记录。
	LimitsDegraded bool
}

// MaxOutputBytes 是单次工具输出的硬上限。工具的参数上界（max_results 等）
// 约束条数，但一条 grep 匹配行本身可达 1 MB，没有字节上限时单个
// tool.result 事件就能把事件日志和界面撑爆。
//
// 与 patch.MaxDiffBytes 同源：都是 limits.MaxPayloadBytes，改一处即可。
const MaxOutputBytes = limits.MaxPayloadBytes

// Tool 是一个可被模型调用的工具。
type Tool interface {
	Spec() Spec
	Prepare(ctx context.Context, call Call) (Prepared, error)
	Execute(ctx context.Context, p Prepared) (Result, error)
}

// Source 是一组同源工具的提供者。Registry 由若干 Source 聚合而成，
// 因此新增一种来源（MCP server、第三方插件）只需要实现这个接口，
// 宿主装配代码不必跟着改。
//
// 契约：Tools 失败只影响该来源，宿主跳过后其余工具照常可用——
// 插件不得成为运行前提（ADR-0009）。
type Source interface {
	// Name 是来源标识，形如 builtin / mcp:foo / plugin:bar；会填进
	// Spec.Source 并随审计落库。
	Name() string
	// Tools 返回该来源当前可用的工具。
	Tools(ctx context.Context) ([]Tool, error)
	// Close 释放来源持有的资源（子进程、连接）。重复调用应当安全。
	Close() error
}

// StaticSource 是已在内存里的工具集合：内置工具，以及按任务构造的
// remember / skill 这类依赖运行期上下文的工具。
type StaticSource struct {
	name  string
	tools []Tool
}

// NewStaticSource 把一组工具打包成来源。
func NewStaticSource(name string, ts ...Tool) *StaticSource {
	return &StaticSource{name: name, tools: ts}
}

// Name 实现 Source。
func (s *StaticSource) Name() string { return s.name }

// Tools 返回副本：调用方通常要 append 进自己的列表，不该动到来源内部状态。
func (s *StaticSource) Tools(context.Context) ([]Tool, error) {
	out := make([]Tool, len(s.tools))
	copy(out, s.tools)
	return out, nil
}

// Close 无事可做：这些工具没有子进程或连接。
func (s *StaticSource) Close() error { return nil }

// sourcedTool 给工具补上来源标识。工具自己声明了 Source 时以它为准
// （例如 MCP 工具要精确到 mcp:<server>）。
type sourcedTool struct {
	Tool
	source string
}

func (t sourcedTool) Spec() Spec {
	spec := t.Tool.Spec()
	if spec.Source == "" {
		spec.Source = t.source
	}
	return spec
}

// Collect 按顺序聚合来源，失败只记录不中断。
func Collect(ctx context.Context, sources ...Source) ([]Tool, []error) {
	var (
		out  []Tool
		errs []error
	)
	for _, src := range sources {
		if src == nil {
			continue
		}
		batch, err := src.Tools(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("tools: 来源 %s 不可用：%w", src.Name(), err))
			continue
		}
		for _, t := range batch {
			if t == nil {
				continue
			}
			out = append(out, sourcedTool{Tool: t, source: src.Name()})
		}
	}
	return out, errs
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

// NewRegistryFrom 从来源聚合注册表。
//
// 与 NewRegistry 的差别：来源失败、工具名为空、跨来源重名都只跳过并记进
// 返回值，不返回错误——MCP / 插件是增强，不能因为一台 server 起不来就让
// 任务构造失败（ADR-0009）。调用方负责把 errs 记进日志。
func NewRegistryFrom(ctx context.Context, sources ...Source) (*Registry, []error) {
	ts, errs := Collect(ctx, sources...)
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		spec := t.Spec()
		if spec.Name == "" {
			errs = append(errs, fmt.Errorf("tools: 来源 %s 有工具未命名，已跳过", spec.Source))
			continue
		}
		if _, ok := r.byName[spec.Name]; ok {
			errs = append(errs, fmt.Errorf("tools: 工具 %q 重复注册（来源 %s），已跳过", spec.Name, spec.Source))
			continue
		}
		r.byName[spec.Name] = t
		r.names = append(r.names, spec.Name)
	}
	sort.Strings(r.names)
	return r, errs
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
