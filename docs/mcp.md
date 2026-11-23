# MCP 工具

jiaoag 可以把 [MCP](https://modelcontextprotocol.io) server 的工具接进同一套工具调用、审批与审计里。实现只覆盖 stdio transport 与 `tools/list` / `tools/call`；协议决策见 ADR-0014。

## 配置

```yaml
mcp:
  servers:
    - name: filesystem
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/me/code"]
      # 传给子进程的环境变量，在继承当前环境的基础上追加。
      env:
        LOG_LEVEL: info
      # 子进程工作目录；默认继承 server 的 cwd。
      dir: ~/code
      # 单次 tools/call 的超时秒数，默认 60，范围 1-600。
      timeout_s: 60
      # allow（默认，配置即信任）或 ask（每次调用都审批）。
      trust: allow
      enabled: true
```

约束：

- `name` 只能包含字母、数字、`_`、`-`，最长 32，且不能重复；它决定工具前缀 `mcp__<name>__<tool>`。
- `command` + `args` 直接 `exec`，**不经过 shell**；需要管道的场景请自己写包装脚本。
- `args` 里的 `~` 不会展开（避免对参数做不必要的解释）；需要绝对路径就写绝对路径。
- 配置校验失败会在启动时直接报错；单台 server 连不上则只记录日志并跳过。

## 工具长什么样

- 模型看到的工具名是 `mcp__filesystem__read_file` 这类形式；重名或清洗后撞名会被跳过。
- `inputSchema` 原样透传给模型；宿主只检查「参数是 object」和「required 键存在」，真正的参数校验由 server 负责。
- 每个 workspace root 首次用到时懒启动一台子进程，之后复用；同一 root 不会重复连。
- 启动超时 10 秒，单次调用超时由 `timeout_s` 控制。调用被取消时通过协议通知 server。

## 审批与信任模型

MCP server 与 jiaoag 同权限运行，**不是沙箱**（ADR-0001）。因此边界画在配置面上：

- **写进配置即信任**：`trust` 默认 `allow`，整台 server 的工具直接执行，不再逐次弹审批。`trust` 只能来自本地配置文件，工具无权自称可信。
- 需要把关的 server 显式写 `trust: ask`，调用改为逐次人工批准，TUI 显示工具名、来源与参数快照（上限 8 KB）：`y` 批准一次、`a` 本会话内同类调用都批准、`n` 拒绝。
- 拒绝时一个 `tools/call` 都不会发出去；批准后任务若已取消，也不会发。
- 两种路径都写审计：`external.allow`（policy，免审执行）、`external.approved`（user / allow_session）、`external.denied`、`external.failed`。detail 含 source、参数、规则版本、耗时。

也就是说：是否弹审批取决于你是否把这台 server 写进配置，而不是每次调用重新确认。加一台会写文件或跑命令的 server，等于把对应的权限交给它自己判断——先读它的工具清单再加。

审计示例：

```sql
SELECT at, actor, action, detail FROM audit WHERE action LIKE 'external.%' ORDER BY id DESC LIMIT 10;
```

## 快速验证

```bash
make demo-mcp
```

需要 `npx`/Node 与 `JIAOAG_DEMO_*`（见 README「真实 provider 冒烟」）。脚本用官方 filesystem server、真实模型跑一轮「列工具 → 外部调用审批 → 执行 → 回填」，并独立核验审计里的 `external.approved`——为了覆盖审批路径，脚本里显式写了 `trust: ask`；默认的 `allow` 只记 `external.allow`。只验证适配器本身时用 `go test -tags mcpsmoke ./internal/mcp/`。

## 失败行为

- server 起不来、`tools/list` 失败：跳过该来源，任务照常开始；只是这些工具不可用。
- 调用中途进程崩溃：该次工具调用返回错误（`tool.result` 记为出错），任务可以继续；不会自动重连。
- 输出按 `MaxOutputBytes` 截断；图片、音频、内嵌资源只留占位符，不把 base64 塞进上下文。

## 不在范围内

HTTP/SSE transport、OAuth、resources / prompts / sampling / elicitation、自动重连、并行工具调用。需要这些能力时按 ADR-0009 的准入清单重新评估，而不是在现有实现上就地加开关。