# jiaoag

单用户的编码自动化 agent。服务端在本机直接操作真实文件系统与 shell，TUI 客户端只负责渲染与审批；两者通过 WebSocket 长连接通信。

- 术语表：`CONTEXT.md`
- 架构与分层：`docs/design/agent-architecture.md`
- 关键取舍：`docs/adr/`
- 实施计划：`docs/plans/v1.md`

## 构建

```bash
go build -o bin/jiaoag-server ./cmd/jiaoag-server
go build -o bin/jiaoag-tui    ./cmd/jiaoag-tui
```

## 配置

```bash
./bin/jiaoag-server -init      # 写入 ~/.config/jiaoag/config.yaml（不覆盖已有文件）
```

随后编辑该文件：`server.token` 与各 `providers[].api_key` 支持三种写法——明文、`$ENV_NAME`、`file:/abs/path`。

## 运行

```bash
export JIAOAG_TOKEN=...
./bin/jiaoag-server            # 默认监听 127.0.0.1:8787
./bin/jiaoag-tui               # q 退出，r 重连
```

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
| M3 | 只读闭环（loop + read/grep/glob） | 未开始 |
| M4 | 变更提案与审批（diff + hash CAS） | 未开始 |
| M5 | `run_command` + 权限策略 + 审计 | 未开始 |
| M6 | 多会话并行、断连继续跑、回放 | 未开始 |

## 安全边界

首版不使用容器，agent 直接作用于真实文件系统。安全边界只有一层：权限策略（工作区路径约束、工具分级、命令默认需审批 + argv 白名单）加客户端审批，且全部工具调用写入审计。详见 `docs/adr/0001` 与 `docs/adr/0004`。
