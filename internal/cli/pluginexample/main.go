// jiaoag plugin-example 是插件协议的参考实现。
//
// 它同时是「怎么写插件」的活文档：一个换行分隔的 JSON-RPC 循环，四个方法
// （initialize / tools/list / prepare / execute），prepare 只声明副作用，
// 真正的写入与命令由宿主执行——插件拿不到绕过审批的能力。
//
// 配到 ~/.config/jiaoag/config.yaml：
//
//	plugins:
//	  servers:
//	    - name: example
//	      command: /absolute/path/to/jiaoag plugin-example
//
// 然后模型就能看到 plugin__example__echo / __save_note / __disk_usage / __ping。
package pluginexample

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const version = "0.1.0"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

func Main() {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintf(os.Stderr, "example-plugin: 解析请求: %v\n", err)
			continue
		}
		res := handle(req)
		if err := enc.Encode(res); err != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

func handle(req request) response {
	out := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		out.Result = map[string]any{
			"name":             "example",
			"version":          version,
			"protocol_version": 1,
		}
	case "tools/list":
		out.Result = map[string]any{"tools": tools()}
	case "prepare":
		var p struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			out.Error = rpcErr{Code: -32602, Message: err.Error()}
			return out
		}
		res, err := prepare(p.Tool, p.Args)
		if err != nil {
			out.Error = rpcErr{Code: -32000, Message: err.Error()}
			return out
		}
		out.Result = res
	case "execute":
		var p struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			out.Error = rpcErr{Code: -32602, Message: err.Error()}
			return out
		}
		res, err := execute(p.Tool, p.Args)
		if err != nil {
			out.Error = rpcErr{Code: -32000, Message: err.Error()}
			return out
		}
		out.Result = res
	default:
		out.Error = rpcErr{Code: -32601, Message: "未知方法 " + req.Method}
	}
	return out
}

func tools() []toolSpec {
	obj := func(s string) json.RawMessage { return json.RawMessage(s) }
	return []toolSpec{
		{
			Name:        "echo",
			Description: "回声：把 text 原样返回，用来验证插件链路。",
			Schema:      obj(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		},
		{
			Name:        "save_note",
			Description: "把一段文字保存为工作区根目录的 note-<name>.md。写入由宿主审批并落盘。",
			Schema:      obj(`{"type":"object","properties":{"name":{"type":"string"},"content":{"type":"string"}},"required":["name","content"]}`),
		},
		{
			Name:        "disk_usage",
			Description: "在工作区根目录跑 du -sh .，用于演示插件声明的命令由宿主执行。",
			Schema:      obj(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "ping",
			Description: "返回 pong，用于演示「插件自己执行」这一类副作用（每次都要审批）。",
			Schema:      obj(`{"type":"object","properties":{}}`),
		},
	}
}

func prepare(tool string, raw json.RawMessage) (map[string]any, error) {
	switch tool {
	case "echo":
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Text) == "" {
			return nil, fmt.Errorf("text 不能为空")
		}
		return map[string]any{"kind": "read"}, nil
	case "save_note":
		var args struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(args.Name)
		if name == "" {
			return nil, fmt.Errorf("name 不能为空")
		}
		return map[string]any{
			"kind":    "write",
			"path":    "note-" + name + ".md",
			"content": args.Content,
		}, nil
	case "disk_usage":
		return map[string]any{"kind": "command", "argv": []string{"du", "-sh", "."}, "timeout_s": 30}, nil
	case "ping":
		return map[string]any{"kind": "external", "summary": "插件返回 pong"}, nil
	default:
		return nil, fmt.Errorf("未知工具 %s", tool)
	}
}

func execute(tool string, raw json.RawMessage) (map[string]any, error) {
	switch tool {
	case "echo":
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		return map[string]any{"content": "插件回声：" + args.Text}, nil
	case "save_note":
		// 到达这里说明宿主没走审批路径，属于宿主 bug；插件不做兜底写入。
		return map[string]any{"content": "写入应由宿主执行", "is_error": true}, nil
	case "ping":
		return map[string]any{"content": "pong from example plugin"}, nil
	default:
		return nil, fmt.Errorf("工具 %s 没有 execute 实现", tool)
	}
}
