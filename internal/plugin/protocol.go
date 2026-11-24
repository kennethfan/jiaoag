// Package plugin 实现第三方 stdio 插件宿主。
//
// 和 MCP 的区别只有一处，但很关键：插件可以把「想做什么」和「怎么落地」分开。
// prepare 返回一份**声明**（读点什么、写哪个文件的新内容、跑哪条命令、或者交给
// 插件自己执行），真正的副作用由宿主做：写文件走 diff 审批 + CAS，命令走宿主
// 选定的执行后端与沙箱，审计按现有通道记录。插件因此拿不到「悄悄改盘」的能力。
//
// 插件是可执行文件，和宿主同权限，**不是沙箱**；信任来自「谁装进 plugins.servers」。
// 协议是本文件里定义的换行分隔 JSON-RPC，不含 HTTP、不含并发请求。
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ProtocolVersion 是插件协议版本。破坏性变更必须递增。
const ProtocolVersion = 1

const (
	methodInitialize = "initialize"
	methodListTools  = "tools/list"
	methodPrepare    = "prepare"
	methodExecute    = "execute"
)

// 副作用种类。
const (
	KindRead     = "read"
	KindWrite    = "write"
	KindCommand  = "command"
	KindExternal = "external"
)

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("插件错误 %d: %s", e.Code, e.Message) }

// Info 是 initialize 的返回。
type Info struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}

// ToolSpec 是插件声明的工具。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// PrepareResult 是副作用声明。按 Kind 只填对应字段。
type PrepareResult struct {
	Kind    string   `json:"kind"`
	Path    string   `json:"path,omitempty"`
	Content string   `json:"content,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Timeout *int     `json:"timeout_s,omitempty"`
	Summary string   `json:"summary,omitempty"`
}

// ExecuteResult 是插件自执行（read / external）的返回。
type ExecuteResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// client 是一条插件连接。协议是单飞的：同一时刻只有一个在途请求，
// 调用方用 ctx 控制等待时间；超时即关闭连接（插件进程由上层杀掉），
// 避免「迟到的响应被下一个请求读走」这类错位。
type client struct {
	rwc io.ReadWriteCloser
	enc *json.Encoder
	dec *json.Decoder

	mu     sync.Mutex
	nextID atomic.Int64
	closed atomic.Bool
}

func newClient(rwc io.ReadWriteCloser) *client {
	return &client{
		rwc: rwc,
		enc: json.NewEncoder(rwc),
		dec: json.NewDecoder(bufio.NewReaderSize(rwc, 64<<10)),
	}
}

func (c *client) call(ctx context.Context, method string, params, out any) error {
	if c.closed.Load() {
		return errors.New("plugin: 连接已关闭")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	id := c.nextID.Add(1)
	req := request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := c.enc.Encode(req); err != nil {
		c.close()
		return fmt.Errorf("plugin: 发送 %s: %w", method, err)
	}

	type outcome struct {
		resp response
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		var resp response
		err := c.dec.Decode(&resp)
		ch <- outcome{resp: resp, err: err}
	}()

	select {
	case <-ctx.Done():
		// 读协程可能还在阻塞；关连接让它退出，并把客户端作废。
		c.close()
		return ctx.Err()
	case got := <-ch:
		if got.err != nil {
			c.close()
			return fmt.Errorf("plugin: 读取 %s 响应: %w", method, got.err)
		}
		if got.resp.Error != nil {
			return got.resp.Error
		}
		if got.resp.ID != id {
			c.close()
			return fmt.Errorf("plugin: 响应 id %d 与请求 %d 不符", got.resp.ID, id)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(got.resp.Result, out); err != nil {
			return fmt.Errorf("plugin: 解析 %s 结果: %w", method, err)
		}
		return nil
	}
}

func (c *client) close() {
	if c.closed.CompareAndSwap(false, true) {
		c.rwc.Close()
	}
}

// initialize 握手并检查协议版本。
func (c *client) initialize(ctx context.Context, hostVersion string) (Info, error) {
	var info Info
	params := map[string]any{"protocol_version": ProtocolVersion, "host": "jiaoag", "host_version": hostVersion}
	if err := c.call(ctx, methodInitialize, params, &info); err != nil {
		return Info{}, err
	}
	if info.ProtocolVersion != ProtocolVersion {
		return Info{}, fmt.Errorf("plugin: 协议版本 %d，宿主只支持 %d", info.ProtocolVersion, ProtocolVersion)
	}
	return info, nil
}

func (c *client) listTools(ctx context.Context) ([]ToolSpec, error) {
	var out struct {
		Tools []ToolSpec `json:"tools"`
	}
	if err := c.call(ctx, methodListTools, struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

func (c *client) prepare(ctx context.Context, tool string, args json.RawMessage) (PrepareResult, error) {
	var out PrepareResult
	params := map[string]any{"tool": tool, "args": args}
	if err := c.call(ctx, methodPrepare, params, &out); err != nil {
		return PrepareResult{}, err
	}
	return out, nil
}

func (c *client) execute(ctx context.Context, tool string, args json.RawMessage) (ExecuteResult, error) {
	var out ExecuteResult
	params := map[string]any{"tool": tool, "args": args}
	if err := c.call(ctx, methodExecute, params, &out); err != nil {
		return ExecuteResult{}, err
	}
	return out, nil
}
