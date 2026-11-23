# jiaoag

单用户的编码自动化 agent。服务端在本机直接操作真实文件系统与 shell，TUI 客户端只负责渲染与审批；两者通过 WebSocket 长连接通信。

- 术语表：`CONTEXT.md`
- 架构与分层：`docs/design/agent-architecture.md`
- 关键取舍：`docs/adr/`
- 实施计划：`docs/plans/v1.md`
- 真实 provider 端到端验收：`docs/real-provider-e2e.md`

## 构建

```bash
go build -o bin/jiaoag-server ./cmd/jiaoag-server
go build -o bin/jiaoag-tui    ./cmd/jiaoag-tui
```

## 静态检查

```bash
make check    # gofmt + go vet + staticcheck + go mod tidy -diff + go test ./...
```

CI（`.github/workflows/ci.yml`）在 push 到 `master` 与 PR 时跑两个 job：`check`（ubuntu，`make check`）与 `windows`（`go build` + `go vet` + `go test ./...`）。Windows 这组会真实执行平台测试，不只是编译。

## 配置

```bash
./bin/jiaoag-server -init      # 写入 ~/.config/jiaoag/config.yaml（不覆盖已有文件）
```

随后编辑该文件：`server.token` 与各 `providers[].api_key` 支持三种写法——明文、`$ENV_NAME`、`file:/abs/path`。

一个 provider 可以配多把 key：把 `api_key` 换成 `api_keys` 列表（写法同上，优先于 `api_key`）。请求在这些 key 之间轮询；鉴权失败、额度不足或限流时该 key 进入冷却，后续请求自动跳过，全部 key 都不可用时才报错。单 key 配置行为不变。

执行类工具的资源上限配在 `exec.limits`（`cpu_seconds` / `memory_mb` / `max_files`，0 表示不限制）：Unix 通过 `sh` 包装的 `ulimit` 施加；Windows 用 Job Object 施加 `cpu_seconds` 与 `memory_mb`，并置 `KILL_ON_JOB_CLOSE`，句柄关闭时连带回收整棵进程树（`max_files` 在 Windows 没有对应机制）。命令结束（正常退出、取消、超时）都会统一收掉整棵进程树——Unix 杀进程组、Windows 用 `taskkill /T`——不留孤儿进程。

请求了限制却施加失败（创建 Job、设置限额、把子进程放进 Job 任一环节不成立）会直接报错，而不是静默按无限制运行；不支持的平台（如 32 位 Windows）在审计里标记 `limits_degraded`。

并发与配额：`agent.max_concurrent_tasks`（默认 4）限制跨会话同时执行的任务数，槽满时任务回队首等待；`agent.max_queue_depth`（默认 16）限制单会话排队深度，超出时 `task.submit` 返回 `queue_full`；`agent.max_rounds` 是单任务轮数上限（默认 24，设为 `0` 表示不限制）；`providers[].rpm` / `tpm` 是每分钟请求数与 token 数上限（0 = 不限），超出配额的调用会等待而不是失败。

## 运行

首次仍需要一份配置（见上）。之后进到想让它改的项目目录，直接启动 TUI：

```bash
cd ~/agent/code/some-project
/Users/kenneth/agent/code/jiaoag/bin/jiaoag-tui
```

TUI 连不上本地服务端时会自动拉起同目录的 `jiaoag-server`（日志写到系统缓存目录下的 `jiaoag/server.log`：macOS 是 `~/Library/Caches/jiaoag/server.log`，Linux 是 `~/.cache/jiaoag/server.log`）。拉起时会给服务端传 `-exit-when-idle=30s`：最后一个客户端断开后 30 秒内没有新连接、也没有排队或运行中的任务，服务端自行优雅退出。所以多开 TUI 时，某一个退出不会踢掉其他人；如果已有服务端在跑则直接复用，也不会误杀。

- `-no-spawn`：不自动拉起，连不上就报错（脚本/CI 用）。
- `-server-bin`：指定 `jiaoag-server` 路径，默认依次找 PATH 与 TUI 同目录。
- `-url` 指向远端地址时不会拉起本地进程。

也可以手工分开跑（手工启动默认常驻，不会空闲退出）：

```bash
./bin/jiaoag-server            # 默认监听 127.0.0.1:8787；需要自动退出时加 -exit-when-idle=30s
./bin/jiaoag-tui   # [enter] 发送  [esc] 取消运行中任务/退出  [ctrl+q] 队列  [/models] 模型  [/skill] 技能  [/mcp] MCP  [/trust] 项目扩展  [!] 本机命令  [ctrl+r] 重连  [ctrl+c] 退出
```

任务按提交顺序串行执行：运行中也可以继续输入，新任务进入会话队列。`ctrl+q` 打开队列面板，`↑/↓` 选择、`x` 取消选中、`D` 再按 `y` 清空；取消要等 `task.status` 终态事件才真正出队。

输入以 `!` 开头时，命令在**客户端本机**执行（`sh -c`，cwd 是工作区），输出逐行刷进对话区，`ctrl+x` 随时终止：

```text
› !go test ./...
! go test ./...
  │ ok  github.com/kennethfan/jiaoag/internal/tui
  ↳ 退出码 0（812 ms）
```

这条路径不经过服务端：不审批、不进审计、不受 `exec.backend` 沙箱约束，也不进入模型上下文——它就是你自己终端里的一条命令。默认 180 秒超时，单次最多展示 400 行（多出的行折叠成「省略 N 行」），连接断开时照样可用。详见 [`docs/local-shell.md`](docs/local-shell.md)。

`/skill` 指定技能：`~/.agents/skills` 下的技能默认只有索引进上下文，正文按需加载；想让某条任务一定用上某个技能（尤其是标了 `disable-model-invocation` 的那些），用 `/skill` 点名，正文会直接进这条任务的系统提示：

```text
› /skill                 # 面板里 ↑↓ 选、enter 指定、x 清除
› /skill grilling        # 或直接写名字
```

只影响下一条任务。详见 [`docs/skills.md`](docs/skills.md)。

仓库也可以自带扩展：根目录 `AGENTS.md`、`.jiaoag/prompts/*.md` 是提示，`.jiaoag/skills` 与 `.agents/skills` 是技能，`.jiaoag/mcp.yaml` 是 MCP server。这些属于**仓库内容**，默认不信任——首次发现时只提示不加载，`/trust` 确认后才生效；内容一改就要重新确认（`/untrust` 撤销，`project.mode: auto` 可跳过确认）：

```text
⚠ 检测到项目级扩展（提示 1 个、MCP 1 台），未确认已跳过；输入 /trust 查看并启用
› /trust
✔ 已信任项目级扩展：提示 1 个、MCP 1 台（下一条任务生效）
  · 提示 AGENTS.md
  · MCP repo-db: ./scripts/mcp-db --readonly
```

详见 [`docs/project-extensions.md`](docs/project-extensions.md)。

无终端环境下自检两端是否互通：

```bash
./bin/jiaoag-tui -check        # 期望输出：ok: 服务端 0.1.0-dev，协议 v1，双向链路正常
curl -s 127.0.0.1:8787/healthz
```

## 真实 provider 冒烟

```bash
make demo
```

一条命令起真实 server，在临时小项目上跑完整一轮「读代码 → 定位 bug → diff 审批 → 落盘 → 跑测试」，最后独立核验磁盘内容、重跑测试并打印审计。审批由脚本按 `allow_once` 裁决，等价于 TUI 里按 `y`。

需要三个环境变量（也可写进 `~/.config/jiaoag/demo.env`，该文件不进仓库）：

```bash
export JIAOAG_DEMO_BASE_URL=https://apihub.agnes-ai.com/v1
export JIAOAG_DEMO_MODEL=agnes-3.0-flash
export JIAOAG_DEMO_API_KEY_FILE=~/.config/agnes/key
make demo
```

可选：`JIAOAG_DEMO_PROVIDER_TYPE`（`openai` 默认 / `anthropic`）、`JIAOAG_DEMO_PORT`（默认 18787）、`JIAOAG_DEMO_EXEC_BACKEND`（`direct` 默认 / `sandbox`）、`JIAOAG_DEMO_KEEP=1`（成功也保留临时目录）。失败时现场自动保留并打印 `server.log` 尾部。

MCP 另有一条冒烟入口：

```bash
make demo-mcp
```

它用官方 `@modelcontextprotocol/server-filesystem`（stdio，经 `npx` 启动）作为外部工具源，prompt 要求「只用 `mcp__fs__read_text_file` 读文件」，脚本据此核验：模型确实选了外部工具、结果回填到对话、`external.approved` 进了审计。可选 `JIAOAG_DEMO_MCP_PORT`（默认 18788）、`JIAOAG_DEMO_MCP_SERVER`（换一个现成 server，跳过 npx 检查）。

## 进度

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M0 | 配置加载、`/healthz`、WS 握手与 ping/pong | ✅ |
| M1 | 事件与存储（7 张表、seq、回放） | ✅ |
| M2 | Provider 适配（openai 兼容 + anthropic） | ✅ |
| M3 | 只读闭环（loop + read/grep/glob + 流式渲染） | ✅ |
| M4 | 变更提案与审批（diff + hash CAS） | ✅ |
| M5 | `run_command` + 参数级权限策略 + 输出落文件 + git 只读 + 审计 | ✅ |
| M6 | 多会话并行、同仓警告、会话内 FIFO 排队、panic 兜底、断连全量回放 | ✅ |
| 加固 | 命令资源限制（Unix rlimit / Windows Job Object）、并发与配额（全局槽 + 队列深度 + provider 令牌桶）、TUI 队列面板、CI 双 job | ✅ |
| 回放 | 增量回放 + 待审批快照（`event.snapshot`），`from_seq` 超前时重置 | ✅ |
| v2.0 | MCP stdio 工具源：外部工具接入、`trust` 信任模型、`external.*` 审计、失败隔离 | ✅ |
| v2.1 | 执行隔离：`exec.backend: sandbox`（macOS seatbelt / Linux bwrap），写白名单 + 密钥禁读，不可用即启动报错 | ✅ |
| v2.2 | 上下文压缩：超预算 80% 把较早轮次摘成 `context.compacted` 事件，保留最近 2 轮，失败降级 | ✅ |
| v2.3 | 重启续跑：`queued` 任务重启后自动恢复（`agent.resume_queued`，默认开），执行中任务仍 canceled | ✅ |
| v2.4 | 跨会话记忆：工作区 `MEMORY.md` 注入 + `remember` 工具（追加走 diff 审批），`agent.memory` 默认开 | ✅ |
| v2.5 | 单 provider 多 key：`api_keys` 轮询，鉴权 / 额度 / 限流失败冷却剔除（ADR-0020） | ✅ |
| v2.6 | 模型列表与切换：`/models` 列候选（配置优先，否则查供应商），选中即写 `session.updated`（ADR-0021） | ✅ |
| v2.7 | 本机命令：TUI 里 `!命令` 直接在客户端执行，流式输出、`ctrl+x` 终止，不审批/不审计/不进上下文（ADR-0022） | ✅ |
| v2.8 | MCP 信任模型：写进配置即放行（`trust` 默认 `allow`），需要把关的 server 显式 `trust: ask` | ✅ |
| v2.9 | 技能包：扫 `~/.agents/skills`，索引常驻、`skill` 工具按需加载正文（ADR-0023） | ✅ |
| v2.10 | 主动调起技能：`disable-model-invocation` 不进索引，`/skill` 指定后正文直接进该任务系统提示（ADR-0024） | ✅ |
| v2.11 | 项目级扩展：工作区 `AGENTS.md` / `.jiaoag/{prompts,skills,mcp.yaml}` / `.agents/skills`，一次确认覆盖三者、内容变化重新确认（ADR-0025） | ✅ |
| v2.12 | MCP 可观测性：`/mcp` 面板（只读，服务器 / 工具数 / 信任 / 版本），提示词申明外部工具返回内容只当数据 | ✅ |

## 安全边界

首版不使用容器，agent 直接作用于真实文件系统。安全边界只有一层：权限策略（工作区路径约束、工具分级、命令默认需审批 + 参数级 argv 白名单）加客户端审批；命令执行与拒绝决定写入 `audit` 表。详见 `docs/adr/0001`、`docs/adr/0004` 与 `docs/adr/0011`。

可选的 `sandbox` 执行后端（`exec.backend: sandbox`）在此之上再加一层进程级隔离：命令只能写工作区、临时目录与工具链缓存，读不到 `~/.ssh` 等密钥目录。它默认关闭，不可用时启动报错而不会静默退回直连；见 [`docs/sandbox.md`](docs/sandbox.md)。

重启行为：排队中的任务默认在服务端重启后自动继续，执行中或等审批的任务不会自动重跑；见 [`docs/restart-resume.md`](docs/restart-resume.md)。

技能包默认从 `~/.agents/skills` 读取：系统提示里只放技能名与适用场景，模型需要时用 `skill` 工具取正文，正文不常驻、也不扩大执行权限；见 [`docs/skills.md`](docs/skills.md)。
