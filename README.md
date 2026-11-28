# jiaoag

单用户的编码自动化 agent。服务端在本机直接操作真实文件系统与 shell，TUI 客户端只负责渲染与审批；两者通过 WebSocket 长连接通信。

- 术语表：`CONTEXT.md`
- 架构与分层：`docs/design/architecture-v3.md`（现行；`agent-architecture.md` 是 v1 历史设计）
- 关键取舍：`docs/adr/`
- 实施计划：`docs/plans/v1.md`
- 真实 provider 端到端验收：`docs/real-provider-e2e.md`

能力和运维各有一份专题文档：`docs/tui.md`（界面与键位）、`docs/tls.md`、`docs/tokens.md`、`docs/observability.md`、`docs/sandbox.md`、`docs/mcp.md`、`docs/plugins.md`、`docs/skills.md`、`docs/project-extensions.md`、`docs/multi-root.md`、`docs/context-compaction.md`、`docs/memory.md`、`docs/restart-resume.md`、`docs/notifications.md`、`docs/policy-feed.md`、`docs/local-shell.md`、`docs/sessions.md`、`docs/web.md`、`docs/write-approval.md`、`docs/commands.md`（命令白名单与后台任务）、`docs/config.md`（配置查看与修改）。分层、契约、多运行时与扩展点的完整说明见 `docs/design/architecture-v3.md`。还剩哪些工作、哪些明确不做，见 `docs/roadmap.md`。

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

默认**不经过网络**：TUI 在自己进程里直接持有运行时（会话、任务、工具执行都在同一个进程），不开端口、不需要令牌、不连 WebSocket。多个运行时可以同时使用同一个数据目录——调度不靠文件锁，而靠库里的**任务占位 + 租约**：启动任务前用一条原子 UPDATE 抢占 `queued` 任务（同一会话天然串行、全局遵守并发上限），执行期间每 10 秒续租，进程崩溃后 60 秒租约过期，任务被标为 `failed(owner_lost)` 而不是重跑。所以两个内嵌 TUI、或 TUI 与 `jiaoag serve` 并存都不会重复执行同一个任务；`serve` 在监听时 TUI 会当客户端，只是因为它能顺便给 web / 远程用。
需要多客户端、浏览器或远程访问时，先起 `jiaoag serve`（日志写到系统缓存目录下的 `jiaoag/server.log`：macOS 是 `~/Library/Caches/jiaoag/server.log`，Linux 是 `~/.cache/jiaoag/server.log`）。TUI 发现数据目录被它占用后会直接连上去，行为和以前一样。没有任何运行时在跑时开 TUI，才会用自己这个二进制拉起 `serve`（带 `-exit-when-idle=30s`，最后一个客户端断开后自行退出）——这条兜底路径只在 `-url` 指向远端或 `-no-spawn` 时才有意义。

- 不带参数打开时，**本目录的会话会全部恢复成标签**，并接到最近更新的那条上（没有会话才新建）；想另开一条新会话用 `/new`。`-s <id>` 直接指定要接的会话。
- `-s <id>`：回到指定会话（历史与工具调用会从事件回放）；`-session` 是同一参数的旧写法。
- `-no-spawn`：不内嵌运行时、也不拉起服务端，只连已有的 `serve`（脚本/CI 用）。
- `-server-bin`：覆盖拉起服务端用的可执行文件（默认就是自己，按 `<bin> serve` 调用）。
- `-insecure`：跳过 TLS 证书校验，只用于自签证书（见下）。
- `-url` 指向远端地址时不会拉起本地进程。

也可以不用终端：服务端起在哪个端口，就用浏览器打开那个地址（`http://127.0.0.1:8787/`，配了 TLS 就是 `https://`）。页面第一次会要求粘贴令牌，之后就是一个和 TUI 对齐的界面——标签恢复本目录会话、提交任务、审批 diff/命令/外部工具（含逐块选择）、管会话与模型、看 MCP 与附加根。唯一的缺口是 `!本机命令`：浏览器里没有 shell，要执行请让 agent 用 `run_command`。页面零构建（原生 HTML + ES module，`go:embed` 进同一个二进制），细节见 [`docs/web.md`](docs/web.md)。

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

界面是「对话 / 面板 / 输入 / 状态栏」四段：默认占满窗口（alternate screen），状态栏钉在终端底行，显示连接状态、工作目录、会话标题、任务状态、轮次、队列、模型、token 与上下文占用（键位提示也在这一行的右侧，输入行紧贴其上）；内容超出视口用 `pgup/pgdn`、`home/end` 滚动，翻页时状态栏提示未读行数。`?` 打开键位与版本面板，`ctrl+f` 在内联/全屏间切换，`-inline` 启动即用内联（保留 scrollback）、鼠标滚轮默认由程序接管（`-no-mouse` 交还终端），按住左键拖动即可框选并复制到剪贴板；键盘用 `ctrl+y` 进入选择模式（`v` 起选、`y` 复制）。审批卡片有三个按钮，`tab` 切换焦点、`enter` 执行，`y`/`a`/`n` 仍可直选。布局与配色见 `docs/tui.md`。

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

写入审批：默认只对**工作区与临时目录之外**、以及工作区里的敏感路径（`.git/`、`.env*`、密钥、`AGENTS.md`、`.jiaoag/`、CI 工作流等）停下来看 diff——工作区内的普通写入直接落盘，只在对话里留一行「自动应用（工作区内）」并写 `write.auto` 审计。想恢复逐次审批设 `agent.write_approval: always`，细节见 [`docs/write-approval.md`](docs/write-approval.md)（ADR-0042）。

需要人工批准时，审批默认是整份文件。diff 有两个以上块时，面板会提示 `[v] 逐块`：进去以后 `space` 勾选、`j/k` 换块、`y` 只应用勾中的块，未勾的块不落盘，工具结果会告诉模型哪几块没写（ADR-0028）。

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

# 常驻采集：写文件、日志轮转后自动重开、断线自动重连并按游标续传
./bin/jiaoag events --follow -output ~/var/jiaoag/events.jsonl -action exec -action write

# 导出历史：事件从 seq=0、审计从 id=0 开始，导出到启动时刻为止后退出
./bin/jiaoag events -session ses_xxx -from 0 -audit-from 0 --until-now > events.jsonl

# 给人看
./bin/jiaoag events -session ses_xxx -from 0 -format text
```

事件行带 `seq`/`type`/`task_id`/`payload`，审计行带 `action`/`actor`/`task_id`/`session_id`/`detail`；审计按 id 游标增量拉取（1 秒一轮，`-interval` 可调）。`--follow` 给常驻采集用：退避重连、按 `seq`/`id` 续传、`-output` 文件被轮转后自动重开；`-action`/`-type` 按前缀过滤（可重复）。详见 [`docs/observability.md`](docs/observability.md)。

无终端环境下自检两端是否互通：

```bash
./bin/jiaoag tui -check        # 期望输出：ok: 服务端 0.1.0-dev，协议 v1，双向链路正常
curl -s 127.0.0.1:8787/healthz
```

## 真实 provider 冒烟

```bash
make demo
```

一条命令起真实 server，在临时工作区上跑三轮，最后独立核验磁盘内容、重跑测试并打印审计。审批由脚本按 `allow_once` 裁决，等价于 TUI 里按 `y`。

1. **读 → 改 → 测**：修 `stats.Sum` 漏加最后一个元素的 bug，出 diff 审批、落盘、跑 `go test ./...`。
2. **插件工具**：调用 `plugin__notes__echo`（示例插件，同二进制 `jiaoag plugin-example`），核验结果回填。
3. **附加根读写**：`-root lib=…` 挂附加根，读 `lib:lib-notes.txt` 再写 `lib:lib-out.txt`，核验主工作区未被污染。

需要三个环境变量（也可写进 `~/.config/jiaoag/demo.env`，该文件不进仓库）：

```bash
export JIAOAG_DEMO_BASE_URL=https://apihub.agnes-ai.com/v1
export JIAOAG_DEMO_MODEL=agnes-3.0-flash
export JIAOAG_DEMO_API_KEY_FILE=~/.config/agnes/key
make demo
```

可选：`JIAOAG_DEMO_PROVIDER_TYPE`（`openai` 默认 / `anthropic`）、`JIAOAG_DEMO_PORT`（默认 18787）、`JIAOAG_DEMO_EXEC_BACKEND`（`direct` 默认 / `sandbox`）、`JIAOAG_DEMO_FULL=0`（只跑第 1 轮）、`JIAOAG_DEMO_KEEP=1`（成功也保留临时目录）。失败时现场自动保留并打印 `server.log` 尾部。

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
| v2.39 | Web 前端：同端口 `/` 挂原生页面（`go:embed`，零构建），复用同一套 WS 协议，范围对齐 TUI 但无 `!本机命令`；静态资源匿名可读、数据接口校验令牌，`/ws` 支持 `?token=`（ADR-0041） | ✅ |
| v2.40 | 写入审批默认只拦工作区外与敏感路径：`agent.write_approval`（workspace/always/off），免审写入仍写 diff、CAS 与 `write.auto` 审计（ADR-0042） | ✅ |
| v2.41 | 构建/打包命令进白名单（go/npm/pnpm/mvn/gradle/make），高风险子命令仍审批；`run_command background=true` + `stop_command` 支持长驻服务（ADR-0043、ADR-0044） | ✅ |
| v2.42 | 白名单可配置：默认规则首次启动落盘到 `policy.d/default.yaml` 并归用户编辑，同目录其他文件只能收紧；`publish`/`deploy` 类子命令是代码底线（ADR-0045） | ✅ |
| v2.43 | 配置子命令：`config path/show/check/set/edit` 与 `policy test`；show 报来源并脱敏，set 只改目标片段、默认 dry-run、写前备份写后校验回滚（ADR-0046） | ✅ |
| v2.44 | 斜杠命令补全：输入 `/` 列出命令并按前缀过滤，`tab` 补全、打全后 `enter` 才执行，浮层计入全屏高度预算（ADR-0047） | ✅ |
| v2.45 | 仅手动技能变成斜杠命令：`/技能名 问题` 一步提交，会话建立后拉取 `skill.list` 进补全（ADR-0024 修订） | ✅ |
| v2.46 | 契约与传输分离：`internal/rpc` 一份契约，TUI 默认进程内直接调用（无端口/令牌/WS）（ADR-0048） | ✅ |
| v2.47 | 多运行时调度：任务占位 + 租约 + 运行时心跳，不再用文件锁；崩溃后标 `owner_lost` 不重跑（ADR-0049） | ✅ |
| v2.48 | 审批跨运行时：裁决 CAS 写 `approvals` + 400ms 轮询，块号随裁决落库；启动清理跳过活着的运行时（ADR-0050） | ✅ |
| v2.49 | 跨运行时事件与后台进程：事件每 250ms 按 seq 从库里补拉，后台进程落库、可跨进程请求停止，属主失联标 `orphaned`（ADR-0051） | ✅ |
| v2.50 | `/ps` 后台进程面板：列出全部会话的进程，运行中可停、孤儿校验启动时间后终止、非运行可删记录（ADR-0052） | ✅ |
| v2.51 | 只读 CLI 直连库：`sessions list` / `events` 不依赖服务端与令牌（ADR-0053） | ✅ |
| v2.52 | 鼠标框选复制：按住左键拖动、松开写入系统剪贴板，软折行拼回同一行，平台命令失败时退回 OSC 52（ADR-0054） | ✅ |
| v2.53 | 通知分级与去重：载荷加 `severity`（审批/失败 critical，完成/取消 info），端点可用 `min_severity` 只看要动手的；同一 `approval_id` 只推一次（修订 ADR-0035） | ✅ |
| v2.54 | 键盘选择复制：`ctrl+y` 进入，hjkl/方向键移动、`v` 起选、`y` 复制，坐标用内容行号可跨屏（修订 ADR-0054） | ✅ |
| v2.55 | Web 端 `/ps`：底部「进程」列全部会话的后台进程，可停止 / 终止孤儿 / 删记录，与 TUI 共用同一套 rpc（修订 ADR-0052） | ✅ |
| v2.56 | 附加根的项目级扩展：`AGENTS.md` / `.jiaoag/` / `MEMORY.md` 与主根一起发现、一次确认；同名技能与 MCP 由主根胜出，`/mcp` 显示 `project:lib`（ADR-0055） | ✅ |
| v2.57 | `jiaoag events --follow`：常驻采集写文件、轮转后自动重开、断线按游标重连续传；`-action`/`-type` 前缀过滤（ADR-0056） | ✅ |
| v2.58 | 上下文估算系数跨任务记忆：按 `provider/model` 落库（`token_ratios`），新任务首轮沿用上次校准（ADR-0057） | ✅ |

## 安全边界

首版不使用容器，agent 直接作用于真实文件系统。安全边界只有一层：权限策略（工作区路径约束、工具分级、命令默认需审批 + 参数级 argv 白名单）加客户端审批；命令执行与拒绝决定写入 `audit` 表。文件写入默认在工作区与临时目录内免审（仍留 diff 事件与 `write.auto` 审计），敏感路径与工作区之外仍审批；见 [`docs/write-approval.md`](docs/write-approval.md)。详见 `docs/adr/0001`、`docs/adr/0004` 与 `docs/adr/0011`。

可选的 `sandbox` 执行后端（`exec.backend: sandbox`）在此之上再加一层进程级隔离：命令只能写工作区、临时目录与工具链缓存，读不到 `~/.ssh` 等密钥目录。它默认关闭，不可用时启动报错而不会静默退回直连；见 [`docs/sandbox.md`](docs/sandbox.md)。

重启行为：排队中的任务默认在服务端重启后自动继续，执行中或等审批的任务不会自动重跑；见 [`docs/restart-resume.md`](docs/restart-resume.md)；人不在电脑前时的审批与终态提醒见 [`docs/notifications.md`](docs/notifications.md)。

技能包默认从 `~/.agents/skills` 读取：系统提示里只放技能名与适用场景，模型需要时用 `skill` 工具取正文，正文不常驻、也不扩大执行权限；见 [`docs/skills.md`](docs/skills.md)。

规则可以来自外部：`jiaoag policy sync` 下载 ed25519 签名的规则 feed，验签后落到 `policy.d/`，服务端启动时加载。feed 只能收紧（把命令变成需要审批），不能放行任何东西，且带有效期与回滚保护；见 [`docs/policy-feed.md`](docs/policy-feed.md)。
