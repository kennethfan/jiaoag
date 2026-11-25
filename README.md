# jiaoag

单用户的编码自动化 agent。服务端在本机直接操作真实文件系统与 shell，TUI 客户端只负责渲染与审批；两者通过 WebSocket 长连接通信。

- 术语表：`CONTEXT.md`
- 架构与分层：`docs/design/agent-architecture.md`
- 关键取舍：`docs/adr/`
- 实施计划：`docs/plans/v1.md`
- 真实 provider 端到端验收：`docs/real-provider-e2e.md`

能力和运维各有一份专题文档：`docs/tui.md`（界面与键位）、`docs/tls.md`、`docs/tokens.md`、`docs/observability.md`、`docs/sandbox.md`、`docs/mcp.md`、`docs/plugins.md`、`docs/skills.md`、`docs/project-extensions.md`、`docs/multi-root.md`、`docs/context-compaction.md`、`docs/memory.md`、`docs/restart-resume.md`、`docs/notifications.md`、`docs/policy-feed.md`、`docs/local-shell.md`、`docs/sessions.md`。还剩哪些工作、哪些明确不做，见 `docs/roadmap.md`。

## 构建

```bash
make build          # 产出唯一的二进制 bin/jiaoag
```

服务端、TUI、会话管理、事件导出、规则 feed、冒烟脚本都是这一个二进制的子命令（`jiaoag help` 看全量，`jiaoag sessions -h` 看子命令自己的用法），早先的 `jiaoag-server` / `jiaoag-tui` / `jiaoag-events` 等独立二进制已移除。

## 静态检查

```bash
make check    # gofmt + go vet + staticcheck + go mod tidy -diff + go test ./...
```

CI（`.github/workflows/ci.yml`）在 push 到 `master` 与 PR 时跑两个 job：`check`（ubuntu，`make check`）与 `windows`（`go build` + `go vet` + `go test ./...`）。Windows 这组会真实执行平台测试，不只是编译。

## 配置

```bash
./bin/jiaoag serve -init   # 写入 ~/.config/jiaoag/config.yaml（不覆盖已有文件）
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
/Users/kenneth/agent/code/jiaoag/bin/jiaoag        # 裸跑就是 TUI
```

TUI 连不上本地服务端时会用自己这个二进制拉起 `serve` 子命令（日志写到系统缓存目录下的 `jiaoag/server.log`：macOS 是 `~/Library/Caches/jiaoag/server.log`，Linux 是 `~/.cache/jiaoag/server.log`）。拉起时会给服务端传 `-exit-when-idle=30s`：最后一个客户端断开后 30 秒内没有新连接、也没有排队或运行中的任务，服务端自行优雅退出。所以多开 TUI 时，某一个退出不会踢掉其他人；如果已有服务端在跑则直接复用，也不会误杀。

- 不带参数打开时，**本目录的会话会全部恢复成标签**，并接到最近更新的那条上（没有会话才新建）；想另开一条新会话用 `/new`。`-s <id>` 直接指定要接的会话。
- `-s <id>`：回到指定会话（历史与工具调用会从事件回放）；`-session` 是同一参数的旧写法。
- `-no-spawn`：不自动拉起，连不上就报错（脚本/CI 用）。
- `-server-bin`：覆盖拉起服务端用的可执行文件（默认就是自己，按 `<bin> serve` 调用）。
- `-insecure`：跳过 TLS 证书校验，只用于自签证书（见下）。
- `-url` 指向远端地址时不会拉起本地进程。

`jiaoag sessions` 与 `jiaoag events` 同样会在本地连不上时自动拉起服务端（同样的 `-no-spawn` / `-server-bin` 开关）：它们只读数据，不该要求先手工起一个服务端。命令结束后服务端仍按 30 秒空闲自行退出。

默认是本机明文连接。要远程访问或跨不可信网络，配置里同时给出证书与私钥即启用 TLS（`wss://`），TUI 会自动跟着换 scheme；自签证书加 `-insecure`。生成命令、反代写法和安全边界见 [`docs/tls.md`](docs/tls.md)：

```yaml
server:
  tls_cert: ~/.config/jiaoag/tls/cert.pem
  tls_key: ~/.config/jiaoag/tls/key.pem
```

令牌可以配多把，并区分只读。只读令牌只能订阅事件、拉审计、列会话、看模型与 MCP 状态，不能提交任务或裁决审批——`jiaoag events` 这类观测入口用它就够：

```yaml
server:
  tokens:
    - name: ops
      value: file:~/.config/jiaoag/token
    - name: viewer
      value: $JIAOAG_READONLY_TOKEN
      read_only: true
```

改完 `kill -HUP <server pid>` 即热重载，连接不断；删掉一条再 HUP 就是吊销。权限矩阵与排障见 [`docs/tokens.md`](docs/tokens.md)。

也可以手工分开跑（手工启动默认常驻，不会空闲退出）：

```bash
./bin/jiaoag serve                            # 默认监听 127.0.0.1:8787；需要自动退出时加 -exit-when-idle=30s
./bin/jiaoag                                  # [enter] 发送  [esc] 取消运行中任务/退出  [ctrl+q] 队列  [/models] 会话模型  [/model] 单次模型  [/skill] 技能  [/mcp] MCP  [/sessions] 会话  [/new] 新会话  [/rename] 改名  [ctrl+n/p] 切标签  [ctrl+w] 关标签  [/trust] 项目扩展  [!] 本机命令  [ctrl+r] 重连  [ctrl+c]/[/exit] 退出

```

界面是「对话 / 面板 / 输入 / 状态栏」四段：默认占满窗口（alternate screen），状态栏钉在终端底行，显示连接状态、工作目录、会话标题、任务状态、轮次、队列、模型、token 与上下文占用（键位提示也在这一行的右侧，输入行紧贴其上）；内容超出视口用 `pgup/pgdn`、`home/end` 滚动，翻页时状态栏提示未读行数。`?` 打开键位与版本面板，`ctrl+f` 在内联/全屏间切换，`-inline` 启动即用内联（保留 scrollback）、鼠标滚轮默认由程序接管（`-no-mouse` 交还终端）。审批卡片有三个按钮，`tab` 切换焦点、`enter` 执行，`y`/`a`/`n` 仍可直选。布局与配色见 `docs/tui.md`。

任务按提交顺序串行执行：运行中也可以继续输入，新任务进入会话队列。`ctrl+q` 打开队列面板，`↑/↓` 选择、`x` 取消选中、`D` 再按 `y` 清空；取消要等 `task.status` 终态事件才真正出队。

`/exit`（或 `ctrl+c`）退出；输入行里的命令都不需要会话，断线时也能用。

`/sessions` 管这个工作区的所有会话：面板按最近活动排序，`↑/↓` 选、`enter` 就地切换（退订旧会话、从 `seq=0` 回放新会话）、`t` 改名（复用输入行，回车保存）、`d` 两次确认删除。有未结束任务的会话既不能切也不能删；删除会把任务、事件、审批、提案、工具调用连同可归属的审计一起清掉，无法撤销。只改当前会话不必开面板：`/rename 新标题` 直接生效，`/rename` 则把现标题填进输入行（`docs/sessions.md`）。

不进 TUI 也能管，用 `jiaoag sessions`：

```bash
jiaoag sessions list                 # 当前目录的会话
jiaoag sessions list -all            # 跨工作区全列（默认只列当前目录）
jiaoag sessions rename ses_8d64 修登录超时
jiaoag sessions rm ses_8d64 -yes     # 不带 -yes 只提醒
```

会话 id 可以只给前几位（要求唯一）。它走 WS 协议，所以要有一个在跑的服务端——`jiaoag` 拉起的那台也算。

写入审批默认是整份文件。diff 有两个以上块时，面板会提示 `[v] 逐块`：进去以后 `space` 勾选、`j/k` 换块、`y` 只应用勾中的块，未勾的块不落盘，工具结果会告诉模型哪几块没写（ADR-0028）。

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

`/model` 同理，只给下一条任务换模型：`/models` 改的是会话默认（之后所有任务都跟着变），`/model` 只覆盖这一条，提交后自动恢复。适合「用便宜模型做一次检索，再用强模型改代码」：

```text
› /model                 # 面板里选，状态行显示「下一条 <model>」
› /model agnes-2.5-flash # 或直接写模型名
```

覆盖会随任务入库（`tasks.provider/model`），重启续跑仍用同一个模型。详见 [`docs/adr/0033-task-model-override.md`](docs/adr/0033-task-model-override.md)。

仓库也可以自带扩展：根目录 `AGENTS.md`、`.jiaoag/prompts/*.md` 是提示，`.jiaoag/skills` 与 `.agents/skills` 是技能，`.jiaoag/mcp.yaml` 是 MCP server。这些属于**仓库内容**，默认不信任——首次发现时只提示不加载，`/trust` 确认后才生效；内容一改就要重新确认（`/untrust` 撤销，`project.mode: auto` 可跳过确认）：

```text
⚠ 检测到项目级扩展（提示 1 个、MCP 1 台），未确认已跳过；输入 /trust 查看并启用
› /trust
✔ 已信任项目级扩展：提示 1 个、MCP 1 台（下一条任务生效）
  · 提示 AGENTS.md
  · MCP repo-db: ./scripts/mcp-db --readonly
```

详见 [`docs/project-extensions.md`](docs/project-extensions.md)。

### 事件与审计导出

`jiaoag events` 是只读消费者：把一个会话的事件流和全库审计增量打到 stdout，默认每行一条 JSON。它没有写入口，接日志系统、CI 或 `jq` 都靠它。

```bash
# 跟着最近活跃的会话 tail（Ctrl+C 退出）
./bin/jiaoag events -config ~/.config/jiaoag/config.yaml

# 导出历史：事件从 seq=0、审计从 id=0 开始，导出到启动时刻为止后退出
./bin/jiaoag events -session ses_xxx -from 0 -audit-from 0 -until-now > events.jsonl

# 给人看
./bin/jiaoag events -session ses_xxx -from 0 -format text
```

事件行带 `seq`/`type`/`task_id`/`payload`，审计行带 `action`/`actor`/`task_id`/`session_id`/`detail`；审计按 id 游标增量拉取（1 秒一轮，`-interval` 可调）。详见 [`docs/observability.md`](docs/observability.md)。

无终端环境下自检两端是否互通：

```bash
./bin/jiaoag tui -check        # 期望输出：ok: 服务端 0.1.0-dev，协议 v1，双向链路正常
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
| v2.13 | MCP 工具过滤：`tools.include/exclude`，配套 LSP 只读接入配方（`docs/mcp.md`） | ✅ |
| v2.14 | 传输加密：可选 `server.tls_cert/tls_key` 启用 wss，TUI `-insecure` 接自签，反代写法见 `docs/tls.md`（ADR-0026） | ✅ |
| v2.15 | 事件与审计导出：`jiaoag events`（只读 WS：`event.head`/`event.subscribe` + `audit.tail` 游标），JSONL 输出（ADR-0027） | ✅ |
| v2.16 | hunk 级部分批准：`approval.resolve` 带 `hunks`，只写勾中的块，未选块丢弃并告知模型（ADR-0028） | ✅ |
| v2.17 | 多令牌与只读权限：`server.tokens[]`（`read_only`），SIGHUP 热重载，吊销不用重启（ADR-0029） | ✅ |
| v2.18 | 收尾三件：`/healthz` 信息分级（匿名只回 ok）、Windows `!命令` 整树终止（Job Object）、`allow_session` 授权落库 + 12h 过期（ADR-0030） | ✅ |
| v2.19 | 上下文预算按估算 token：`agent.context_budget_tokens`（默认 32000）+ 按 `prompt_tokens` 在线校准，旧 `context_budget_chars` 保留折算（ADR-0031） | ✅ |
| v2.20 | 签名规则 feed：`jiaoag policy sync` 验签（ed25519）后落 `policy.d/feed.yaml`，只收紧、带有效期与回滚保护（ADR-0032） | ✅ |
| v2.21 | 按任务选模型：`task.submit` 带 provider/model 只覆盖这一次，TUI 用 `/model` 绑定下一条任务，`jiaoag demo -task-model`（ADR-0033） | ✅ |
| v2.22 | 第三方 stdio 插件：`plugins.servers`，插件只声明意图，写入走 diff 审批、命令走执行后端、失败隔离（ADR-0034） | ✅ |
| v2.23 | Webhook 通知：`notify.webhooks[]`，只推审批与任务终态，异步 + 3 次重试 + 可选 HMAC 签名（ADR-0035） | ✅ |
| v2.24 | 多仓库只读附加根：`-root alias=path`，`read_file`/`grep`/`glob` 用 `alias:path` 访问，写入与命令仍限主根（ADR-0036） | ✅ |
| v2.25 | 附加根可写：写入走同一套 diff/审批/CAS，`run_command root=` 切换 cwd，sandbox 可写范围扩到所有根（ADR-0037） | ✅ |
| v2.26 | TUI 改版：状态栏置底（连接/工作目录/状态/轮次/队列/模型/token）、语义色与 `NO_COLOR` 降级、审批三按钮 + `tab` 焦点、`?` 键位面板、默认全屏 + 内部滚动（`-inline`/`-no-mouse`）（docs/tui.md） | ✅ |
| v2.27 | `run_command` 的 `shell` 入口：整条命令由 `sh -c` 解释，固定审批且理由为 `shell: …`；审批面板展示整条命令，审计带 `shell` 标记（ADR-0038） | ✅ |
| v2.28 | 会话管理：`/sessions` 面板（切换/改名/删除），删除按序清关联数据，忙会话拒绝切换与删除（ADR-0039） | ✅ |
| v2.29 | 命令行会话管理：`jiaoag sessions list/rename/rm`，`-all` 跨工作区、id 前缀补全、删除需 `-yes`（ADR-0039 修订） | ✅ |
| v2.30 | 命令行收口：七个二进制合并为 `jiaoag` 一个入口（`serve` / `tui` / `sessions` / `events` / `policy` / `demo` / `plugin-example`），裸跑即 TUI；旧名 `jiaoag-*` 移除 | ✅ |
| v2.31 | 修空闲退出：只认执行中与真正在内存队列里等槽的任务，库里孤立的 `queued` 不再把服务端拖住不退（ADR-0019 修订） | ✅ |
| v2.32 | `sessions` / `events` 也会在本地连不上时自动拉起服务端（复用 TUI 那套 spawn + 空闲退出，`-no-spawn` 可关） | ✅ |
| v2.33 | 每个子命令自带 `-h`：一句话说明 + 用法 + 子命令 + 标志 + 示例，裸跑子命令也打印帮助（`jiaoag sessions` / `jiaoag policy`） | ✅ |
| v2.34 | 状态栏显示会话标题（没起名就不显示，也不拿 id 占位），改名（本端或命令行）立即生效 | ✅ |
| v2.35 | TUI 加 `/rename`：`/rename <标题>` 直接改当前会话，不带参数则把现标题填进输入行 | ✅ |
| v2.36 | TUI 支持 `/exit`（等同 `ctrl+c`）：命令在会话检查之前处理，断线或未建会话时同样能退 | ✅ |
| v2.37 | 状态栏右侧三件：模型（会话未指定时取 `assistant.start` 回报的本轮实际模型）、累计 token、`ctx 5.2k/32k` 上下文占用（估算值，含系统提示与工具清单，每轮按 `prompt_tokens` 校准）；顺带修上下文超限重试时预算没真正收缩的 bug | ✅ |
| v2.38 | 同窗口多会话标签：启动时恢复本目录全部会话为标签（接最近那条），`/new` 新开一条，`/sessions` 里 `enter` 把会话开成标签（在跑的也能开），`ctrl+n`/`ctrl+p` 循环、`ctrl+w` 关闭；后台会话继续跑，标签上显示进行中/排队/待审批/未读徽标，切回去从 `seq=0` 重放 | ✅ |

## 安全边界

首版不使用容器，agent 直接作用于真实文件系统。安全边界只有一层：权限策略（工作区路径约束、工具分级、命令默认需审批 + 参数级 argv 白名单）加客户端审批；命令执行与拒绝决定写入 `audit` 表。详见 `docs/adr/0001`、`docs/adr/0004` 与 `docs/adr/0011`。

可选的 `sandbox` 执行后端（`exec.backend: sandbox`）在此之上再加一层进程级隔离：命令只能写工作区、临时目录与工具链缓存，读不到 `~/.ssh` 等密钥目录。它默认关闭，不可用时启动报错而不会静默退回直连；见 [`docs/sandbox.md`](docs/sandbox.md)。

重启行为：排队中的任务默认在服务端重启后自动继续，执行中或等审批的任务不会自动重跑；见 [`docs/restart-resume.md`](docs/restart-resume.md)；人不在电脑前时的审批与终态提醒见 [`docs/notifications.md`](docs/notifications.md)。

技能包默认从 `~/.agents/skills` 读取：系统提示里只放技能名与适用场景，模型需要时用 `skill` 工具取正文，正文不常驻、也不扩大执行权限；见 [`docs/skills.md`](docs/skills.md)。

规则可以来自外部：`jiaoag policy sync` 下载 ed25519 签名的规则 feed，验签后落到 `policy.d/`，服务端启动时加载。feed 只能收紧（把命令变成需要审批），不能放行任何东西，且带有效期与回滚保护；见 [`docs/policy-feed.md`](docs/policy-feed.md)。
