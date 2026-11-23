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

执行类工具的资源上限配在 `exec.limits`（`cpu_seconds` / `memory_mb` / `max_files`，0 表示不限制）：Unix 通过 `sh` 包装的 `ulimit` 施加；Windows 用 Job Object 施加 `cpu_seconds` 与 `memory_mb`，并置 `KILL_ON_JOB_CLOSE`，句柄关闭时连带回收整棵进程树（`max_files` 在 Windows 没有对应机制）。命令结束（正常退出、取消、超时）都会统一收掉整棵进程树——Unix 杀进程组、Windows 用 `taskkill /T`——不留孤儿进程。

请求了限制却施加失败（创建 Job、设置限额、把子进程放进 Job 任一环节不成立）会直接报错，而不是静默按无限制运行；不支持的平台（如 32 位 Windows）在审计里标记 `limits_degraded`。

并发与配额：`agent.max_concurrent_tasks`（默认 4）限制跨会话同时执行的任务数，槽满时任务回队首等待；`agent.max_queue_depth`（默认 16）限制单会话排队深度，超出时 `task.submit` 返回 `queue_full`；`providers[].rpm` / `tpm` 是每分钟请求数与 token 数上限（0 = 不限），超出配额的调用会等待而不是失败。

## 运行

```bash
export JIAOAG_TOKEN=...
./bin/jiaoag-server            # 默认监听 127.0.0.1:8787
./bin/jiaoag-tui   # [enter] 发送  [esc] 取消运行中任务/退出  [ctrl+q] 队列  [ctrl+r] 重连  [ctrl+c] 退出
```

任务按提交顺序串行执行：运行中也可以继续输入，新任务进入会话队列。`ctrl+q` 打开队列面板，`↑/↓` 选择、`x` 取消选中、`D` 再按 `y` 清空；取消要等 `task.status` 终态事件才真正出队。

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

可选：`JIAOAG_DEMO_PROVIDER_TYPE`（`openai` 默认 / `anthropic`）、`JIAOAG_DEMO_PORT`（默认 18787）、`JIAOAG_DEMO_KEEP=1`（成功也保留临时目录）。失败时现场自动保留并打印 `server.log` 尾部。

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

## 安全边界

首版不使用容器，agent 直接作用于真实文件系统。安全边界只有一层：权限策略（工作区路径约束、工具分级、命令默认需审批 + 参数级 argv 白名单）加客户端审批；命令执行与拒绝决定写入 `audit` 表。详见 `docs/adr/0001`、`docs/adr/0004` 与 `docs/adr/0011`。
