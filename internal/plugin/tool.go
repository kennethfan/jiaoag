package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	execpkg "github.com/kennethfan/jiaoag/internal/exec"
	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

const (
	// maxPluginContent 是插件单次写入内容的上限。
	maxPluginContent = 1 << 20
	// 命令参数上限与内置 run_command 保持一致，避免两套标准。
	maxArgvTokens = 128
	maxTokenBytes = 4 << 10
	minTimeout    = time.Second
	maxTimeout    = 600 * time.Second
)

// nodeTool 是把一个插件工具适配成 tools.Tool。
//
// Effects 固定为 read：真正的副作用由 Prepare 的返回值决定（Change / Command /
// External 各自进审批通道），这里声明 process 或 propose_write 会让只读调用也
// 被要求审批。
type nodeTool struct {
	server  string
	remote  string
	display string
	spec    tools.Spec
	client  *client
	ws      *workspace.Workspace
	env     tools.RuntimeEnv
	log     *slog.Logger
	timeout time.Duration
}

func newNodeTool(server, pluginVersion, remote, display string, spec ToolSpec, cl *client,
	ws *workspace.Workspace, env tools.RuntimeEnv, log *slog.Logger, timeout time.Duration) *nodeTool {
	schema := schemaBytes(spec.Schema)
	if pluginVersion == "" {
		pluginVersion = "unknown"
	}
	version := "plugin:" + server + "@" + pluginVersion + "#" + shortHash(schema)
	desc := strings.TrimSpace(spec.Description)
	if desc == "" {
		desc = "插件 " + server + " 提供的工具 " + remote
	}
	return &nodeTool{
		server:  server,
		remote:  remote,
		display: display,
		spec: tools.Spec{
			Name:        display,
			Description: desc,
			Schema:      schema,
			Effects:     []tools.Effect{tools.EffectRead},
			Source:      "plugin:" + server,
			Version:     version,
		},
		client:  cl,
		ws:      ws,
		env:     env,
		log:     log,
		timeout: timeout,
	}
}

func (t *nodeTool) Spec() tools.Spec { return t.spec }

// Prepare 只向插件要「声明」，不执行副作用。
func (t *nodeTool) Prepare(ctx context.Context, call tools.Call) (tools.Prepared, error) {
	args, err := normalizeArgs(call.Args)
	if err != nil {
		return tools.Prepared{}, fmt.Errorf("%s: %w", call.Name, err)
	}
	cctx, cancel := t.callContext(ctx)
	defer cancel()

	res, err := t.client.prepare(cctx, t.remote, args)
	if err != nil {
		return tools.Prepared{}, fmt.Errorf("%s: %w", call.Name, err)
	}

	switch res.Kind {
	case KindRead:
		return tools.Prepared{Call: call, Spec: t.spec, Args: args}, nil
	case KindWrite:
		change, abs, err := t.buildChange(res.Path, []byte(res.Content))
		if err != nil {
			return tools.Prepared{}, fmt.Errorf("%s: %w", call.Name, err)
		}
		return tools.Prepared{Call: call, Spec: t.spec, Args: args, Paths: []string{abs}, Change: change}, nil
	case KindCommand:
		cmd, err := t.buildCommand(res)
		if err != nil {
			return tools.Prepared{}, fmt.Errorf("%s: %w", call.Name, err)
		}
		return tools.Prepared{Call: call, Spec: t.spec, Args: args, Paths: []string{cmd.Cwd}, Command: cmd}, nil
	case KindExternal:
		return tools.Prepared{
			Call: call, Spec: t.spec, Args: args,
			External: &tools.External{Source: "plugin:" + t.server},
		}, nil
	default:
		return tools.Prepared{}, fmt.Errorf("%s: 插件返回了未知的副作用类型 %q", call.Name, res.Kind)
	}
}

// Execute 处理 read / external；write 由宿主落盘，command 由宿主执行。
func (t *nodeTool) Execute(ctx context.Context, p tools.Prepared) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	switch {
	case p.Change != nil:
		return tools.Result{
			IsError: true,
			Content: t.display + " 的写入必须经过审批；未审批的调用不会落盘。",
		}, nil
	case p.Command != nil:
		return t.runCommand(ctx, p.Command, p.LogPath)
	case p.External != nil:
		args, _ := p.Args.(json.RawMessage)
		cctx, cancel := t.callContext(ctx)
		defer cancel()
		out, err := t.client.execute(cctx, t.remote, args)
		if err != nil {
			return tools.Result{}, fmt.Errorf("%s: %w", t.display, err)
		}
		return tools.Result{Content: out.Content, IsError: out.IsError}, nil
	default:
		args, _ := p.Args.(json.RawMessage)
		cctx, cancel := t.callContext(ctx)
		defer cancel()
		out, err := t.client.execute(cctx, t.remote, args)
		if err != nil {
			return tools.Result{}, fmt.Errorf("%s: %w", t.display, err)
		}
		return tools.Result{Content: out.Content, IsError: out.IsError}, nil
	}
}

func (t *nodeTool) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := t.timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// buildChange 把插件给的「路径 + 新内容」变成待审批的 Change。
// diff、hash、CAS 全在这里算，插件不参与。
func (t *nodeTool) buildChange(rel string, content []byte) (*tools.Change, string, error) {
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "" || clean == "." {
		return nil, "", fmt.Errorf("插件没有给出写入路径")
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("写入路径 %q 必须落在工作区内", rel)
	}
	if len(content) > maxPluginContent {
		return nil, "", fmt.Errorf("写入内容 %d 字节，超过上限 %d", len(content), maxPluginContent)
	}
	abs, err := t.ws.Resolve(clean)
	if err != nil {
		return nil, "", err
	}
	old, exists, err := t.ws.Snapshot(clean)
	if err != nil {
		return nil, "", err
	}
	if exists && bytes.Equal(old, content) {
		return nil, "", fmt.Errorf("%s 的内容没有变化，不需要写入", clean)
	}
	diff, truncated, err := patch.Unified(clean, old, content, exists)
	if err != nil {
		return nil, "", err
	}
	if diff == "" {
		return nil, "", fmt.Errorf("%s 与原文只差结尾换行，行级 diff 无法展示这种改动", clean)
	}
	change := &tools.Change{
		Path:       clean,
		Rel:        clean,
		Exists:     exists,
		NewHash:    patch.Hash(content),
		NewContent: content,
		Diff:       diff,
		Truncated:  truncated,
	}
	if exists {
		change.BaseHash = patch.Hash(old)
	}
	return change, abs, nil
}

func (t *nodeTool) buildCommand(res PrepareResult) (*tools.Command, error) {
	if len(res.Argv) == 0 {
		return nil, fmt.Errorf("插件声明了命令但没有 argv")
	}
	if len(res.Argv) > maxArgvTokens {
		return nil, fmt.Errorf("argv 有 %d 个 token，超过上限 %d", len(res.Argv), maxArgvTokens)
	}
	for i, token := range res.Argv {
		if token == "" {
			return nil, fmt.Errorf("argv[%d] 为空", i)
		}
		if len(token) > maxTokenBytes {
			return nil, fmt.Errorf("argv[%d] 有 %d 字节，超过上限 %d", i, len(token), maxTokenBytes)
		}
		if strings.ContainsRune(token, 0) {
			return nil, fmt.Errorf("argv[%d] 含 NUL 字节", i)
		}
	}
	timeout := t.env.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if res.Timeout != nil {
		d := time.Duration(*res.Timeout) * time.Second
		if d < minTimeout || d > maxTimeout {
			return nil, fmt.Errorf("timeout_s 必须在 1-600 之间，收到 %d", *res.Timeout)
		}
		timeout = d
	}
	if timeout < minTimeout {
		timeout = minTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	// cwd 固定为工作区根：插件给的 cwd 不采信，否则等于给了个绕过
	// 「path_args 必须在工作区内」的口子。
	backend := ""
	if t.env.Backend != nil {
		backend = t.env.Backend.Name()
	}
	return &tools.Command{
		Argv:    append([]string(nil), res.Argv...),
		Cwd:     t.ws.Root(),
		Timeout: timeout,
		Env:     tools.CommandEnv(os.Environ()),
		Backend: backend,
	}, nil
}

func (t *nodeTool) runCommand(ctx context.Context, cmd *tools.Command, logPath string) (tools.Result, error) {
	if t.env.Backend == nil {
		return tools.Result{}, errNoBackend
	}
	plan, err := t.env.Backend.Prepare(ctx, execpkg.Command{
		Argv:    cmd.Argv,
		Cwd:     cmd.Cwd,
		Timeout: cmd.Timeout,
		Env:     cmd.Env,
		Limits:  t.env.Limits,
	})
	if err != nil {
		return tools.Result{}, err
	}
	started := time.Now()
	res, err := t.env.Backend.Run(ctx, plan, execpkg.SpecFor(logPath, t.env.EventLimit, t.env.LogLimit))
	if err != nil {
		return tools.Result{}, err
	}
	out := tools.Result{
		Content:        strings.TrimRight(res.Output, "\n"),
		Truncated:      res.Truncated,
		ExitCode:       &res.ExitCode,
		OutputPath:     res.OutputPath,
		BackendVersion: plan.Version,
		TimedOut:       res.TimedOut,
		LogError:       res.LogError,
		Limits:         plan.Limits,
		LimitsDegraded: plan.LimitsDegraded(),
		Elapsed:        time.Since(started),
	}
	if out.Content == "" {
		out.Content = "（命令没有输出）"
	}
	out.IsError = res.ExitCode != 0
	if res.TimedOut {
		out.Content += fmt.Sprintf("\n[命令在 %s 后超时]", cmd.Timeout)
	}
	return out, nil
}

// normalizeArgs 保证参数是 JSON object；插件拿不到别的形状。
func normalizeArgs(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("参数必须是 JSON object: %w", err)
	}
	return raw, nil
}

func shortHash(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:4])
}
