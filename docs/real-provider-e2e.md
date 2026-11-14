# 真实 provider 端到端验收

单元测试里的 provider 都是假实现：不校验 SSE 分块、不校验 `tool_calls` 的真实 JSON 形状，也发现不了「适配器把多段 delta 当整段」这类问题。本文的验收用**真实模型**跑完整链路（WS 连接 → 会话 → 任务 → 工具循环 → 审批 → 落盘/执行），用来在升级适配器或换供应商时确认没有回归。

## 开关

单一入口是 `internal/server/real_e2e_test.go`，首行带构建标签：

```go
//go:build realprovider
```

因此默认的 `go test ./...` 与 `go vet ./...` **看不到**这个文件，CI 不会因为缺少密钥而跳过或失败。它只在你显式带上 `-tags realprovider` 时编译。

## 环境变量

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `JIAOAG_E2E_BASE_URL` | 是 | OpenAI 兼容端点，例如 `https://openrouter.ai/api/v1` |
| `JIAOAG_E2E_API_KEY` | 是 | API key；只从环境读取，不读配置文件 |
| `JIAOAG_E2E_MODEL` | 是 | 模型 id，例如 `z-ai/glm-5.3-flash` |
| `JIAOAG_E2E_PROVIDER` | 否 | `openai`（默认）或 `anthropic` |

缺任一必填项时三个用例都会 `t.Skip`，不会误报失败。

密钥只进进程环境：不要写进仓库里的文件，不要写进命令历史（下面用 `$(cat ...)` 在子进程里取），也不要把 key 粘进测试输出。测试失败时的 Fatal 文案只带事件名、工具结果与提案状态，不打印请求头。

## 运行

```bash
JIAOAG_E2E_BASE_URL=https://openrouter.ai/api/v1 \
JIAOAG_E2E_API_KEY=$(cat ~/.config/openrouter/key) \
JIAOAG_E2E_MODEL=z-ai/glm-5.3-flash \
go test -tags realprovider -count=1 -run TestRealProvider -v ./internal/server/
```

一次完整跑约 20 秒（三条用例各 5 秒上下）。测试内起的是真实服务端（`httptest` + 内存 SQLite），用 WS 客户端订阅 `from_seq=0` 全量回放，收到 `approval.requested` 时按本任务 id 自动 `approval.resolve{allow_once}`，直到该任务 `task.status` 进入终态。

## 三条用例各断言什么

| 用例 | 场景 | 关键断言 |
| --- | --- | --- |
| `TestRealProviderReadOnly` | 让模型读取临时目录里的 `notes.txt` 并回报首行 | 任务 `completed`；存在 `read_file` 的 `tool.result` 且内容含文件原文；**没有任何审批** |
| `TestRealProviderWriteApproval` | 让模型用 `write_file` 改写该文件 | 任务 `completed`；出现 `approval.requested{kind: diff}`；批准后 `proposal.applied` 的 `path` 落在 `notes.txt` 上；磁盘内容确实变了 |
| `TestRealProviderCommandApproval` | 让模型执行 `echo jiaoag-e2e` | 任务 `completed`；出现 `approval.requested{kind: command}` 且 `argv/cwd/backend` 齐全；至少一次 `run_command` 结果 `exit_code=0` 且 stdout 含 `jiaoag-e2e` |

超时语义：单次任务整轮预算是 5 分钟（`taskBudget`），两帧之间最多等 120 秒；超时失败会带上已收到的事件名与提案拒绝原因，便于区分「模型不听话」与「服务端卡住」。

## 什么时候必须跑

- 改 `internal/llm`（适配器、重试、超时装饰器）
- 改 `internal/tools` 的工具签名或结果格式
- 改 `internal/app` 的任务/审批接线（`RuntimeDeps`、`ConfigureRuntime`）
- 换供应商或模型（尤其确认 `tool_calls` 增量拼装的实现差异）

这些包的公开结构变动后，即使不打算真跑，也建议补一次编译与静态检查：

```bash
go vet -tags realprovider ./internal/server/
go build -tags realprovider ./internal/server/
```

## 已知环境限制

部分网关（例如某些国内中转）会超时或被防火墙拦掉，表现为 `llm: 暂时性失败` 加 `dial tcp ... i/o timeout`。这类失败在重试后仍会落到任务的 `failed` 终态——属于网络问题，先换一个可达的端点再判断是否为回归。
