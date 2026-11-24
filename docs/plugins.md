# 第三方 stdio 插件

插件是一个普通可执行文件，通过 stdin/stdout 说一个很小的 JSON-RPC 协议。它和 MCP 的
区别只有一处，但很关键：**插件只声明「想做什么」，副作用由宿主落地**。

| | MCP server | 插件 |
|---|---|---|
| 读 | 自己读 | 自己读（`read`） |
| 写文件 | 自己写，宿主只能整台信任或每次都点 | 宿主算 diff、审批、CAS 后落盘（`write`） |
| 起进程 | 自己起，绕过 `exec.backend` | 宿主选后端（direct/sandbox）、环境白名单、超时（`command`） |
| 其他副作用 | 每次审批 | 每次审批（`external`） |
| 工具名 | `mcp__<server>__<tool>` | `plugin__<server>__<tool>` |

所以插件适合「接入你自己的领域工具」：解析某个格式、调内部 API、生成报告；凡是需要
落盘或跑命令的动作，交给宿主做。

## 配置

```yaml
plugins:
  servers:
    - name: notes                     # 进工具名：plugin__notes__<tool>
      command: jiaoag                 # 必须在 PATH 里，或写绝对路径；不经 shell
      args: ["plugin-example"]        # 参考插件就是同一个二进制的子命令
      env:
        LOG_LEVEL: info               # 在继承环境的基础上追加
      dir: ~/code                     # 子进程工作目录，默认继承 server
      timeout_s: 60                   # 单次 prepare/execute 超时，0-600
      enabled: true
```

约束（启动时校验，不合格直接拒绝启动）：

- `name` 只允许字母、数字、`_`、`-`，不超过 32 字节，不能重复；
- `command` 不能为空；
- 插件起不来、握手版本不符、工具重名都只跳过该插件/该工具，不会让任务失败。

## 协议

换行分隔的 JSON-RPC 2.0，`ProtocolVersion = 1`。宿主一次只发一个请求（串行），
没有并发与流式。

### `initialize`

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocol_version":1,"host":"jiaoag","host_version":"0.1.0"}}
```

插件回：

```json
{"jsonrpc":"2.0","id":1,"result":{"name":"example","version":"0.1.0","protocol_version":1}}
```

版本不等即视为不兼容，跳过该插件。

### `tools/list`

```json
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
```

```json
{"jsonrpc":"2.0","id":2,"result":{"tools":[
  {"name":"echo","description":"回声","schema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}
]}}
```

`schema` 会原样作为工具参数说明；不是 JSON object 时回退成 `{"type":"object"}`。
一个插件最多 200 个工具。

### `prepare`

```json
{"jsonrpc":"2.0","id":3,"method":"prepare","params":{"tool":"save_note","args":{"name":"a","text":"hi"}}}
```

`result` 按 `kind` 取四种形状之一：

```json
{"kind":"read"}
{"kind":"write","path":"note-a.md","content":"hi\n"}
{"kind":"command","argv":["du","-sh","."],"timeout_s":30}
{"kind":"external"}
```

`summary` 可选，用于审批面板/日志的可读描述（命令类会被忽略，界面显示真实 argv）。

写入的父目录必须已存在（与内置 `write_file` 一致，宿主不隐式建目录）；路径必须在
工作区内，绝对路径与 `..` 一律拒绝。

### `execute`

只有 `read` 与 `external` 会被宿主调用：

```json
{"jsonrpc":"2.0","id":4,"method":"execute","params":{"tool":"echo","args":{"text":"hi"}}}
```

```json
{"jsonrpc":"2.0","id":4,"result":{"content":"插件回声：hi","is_error":false}}
```

`write` 的 `execute` 会直接报错（「写入必须经过审批」）——插件不应该指望自己落盘；
`command` 也不调用插件，宿主自己跑。

## 宿主怎么处理四类副作用

| kind | 宿主动作 |
|---|---|
| `read` | 直接调 `execute`，返回内容；记 `tool.result` |
| `write` | 校验路径在工作区内、内容 ≤1MB → 取旧内容 → 算 unified diff → 走审批（可逐块）→ CAS 后写入；审计 `write.applied` |
| `command` | 固定 `cwd` 为工作区根、忽略插件给的 cwd；环境白名单；超时夹在 1–600s；经 `exec.backend` 执行；审计 `exec.*` |
| `external` | 每次调用都要审批（`allow_session` 的 12h 便利仍可用）；审计 `external.*` |

注册表里这些工具的 `Effects` 一律是 `read`：副作用由 `prepare` 结果表达。若声明成
process/propose_write，纯读调用也会被迫审批。

## 写一个插件

参考实现：`cmd/jiaoag`（`make build` 会产出 `bin/jiaoag`）。
它包含四个工具：`echo`（读）、`save_note`（在工作区根目录写 `note-<name>.md`）、`disk_usage`
（命令 `du -sh .`）、`ping`（external）。

最小骨架（任意语言）：

```text
读一行 → JSON 解析 → 按 method 分支 → 打一行 JSON 回应
```

- 标准输出只能写响应，日志写标准错误（stderr 会被继承到 server 的日志里）；
- 收到 `initialize` 必须回 `protocol_version: 1`；
- `prepare` 遇到不认识的工具或参数，回 JSON-RPC error（宿主会把它变成工具错误）；
- 不需要处理并发：宿主保证同一时刻只有一个在途请求。

## 安全边界

- **插件不是沙箱**。它和 server 同权限运行，能读环境变量、能自己起进程。宿主中介的是
  「副作用」，不是「代码执行」。要限制插件本身，请把 server 整体放进 sandbox。
- `write` / `command` / `external` 都经过宿主，所以审计里有据可查；`read` 没有额外确认，
  但返回值仍属不可信输入。
- 工具名带 `plugin__` 前缀，模型能分清来源。

## 观测与排障

```bash
# 装了什么、用了哪些工具
sqlite3 ~/.local/share/jiaoag/jiaoag.db \
  "SELECT json_extract(payload,'\$.tool') AS tool FROM events
   WHERE type='tool.requested' AND json_extract(payload,'\$.tool') LIKE 'plugin__%' LIMIT 20;"

# 插件写过的文件
sqlite3 ~/.local/share/jiaoag/jiaoag.db \
  "SELECT ts, json_extract(detail,'\$.path') FROM audit WHERE action='write.applied';"
```

server 日志里会有 `插件已就绪 plugin=<name> tools=<n>`；起不来会有一条 warn，附 stderr
尾部，通常直接就是插件自己的报错。

## 已知限制

- 单飞：一个插件同一时刻只有一个调用，长任务会串行。
- 无流式输出；超时后连接作废（下次调用会重建），因为迟到的响应没法安全配对。
- 不能热重载：改插件实现或配置都要重启 server。
- 写入一次最多 1MB；命令 argv 最多 128 个 token、单个 token 最多 4KB。
- 不支持 resources/prompts/sampling，也不做 HTTP 传输。