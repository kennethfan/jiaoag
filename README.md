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

CI（`.github/workflows/ci.yml`）在 push 到 `master` 与 PR 时执行同一个目标。

## 配置

```bash
./bin/jiaoag-server -init      # 写入 ~/.config/jiaoag/config.yaml（不覆盖已有文件）
```

随后编辑该文件：`server.token` 与各 `providers[].api_key` 支持三种写法——明文、`$ENV_NAME`、`file:/abs/path`。

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

## 安全边界

首版不使用容器，agent 直接作用于真实文件系统。安全边界只有一层：权限策略（工作区路径约束、工具分级、命令默认需审批 + 参数级 argv 白名单）加客户端审批；命令执行与拒绝决定写入 `audit` 表。详见 `docs/adr/0001`、`docs/adr/0004` 与 `docs/adr/0011`。
