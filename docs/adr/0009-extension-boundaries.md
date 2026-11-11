# 扩展边界：只在边缘可插，不变量不可插

项目只服务一个人，但工具、供应商、界面都值得让别人（或以后的自己）加。同时安全模型成立的前提很硬：没有容器，权限策略是唯一边界（ADR-0001），事件日志是历史事实的唯一来源（ADR-0005），副作用只在执行阶段发生（ADR-0006）。如果把这些也开放出去，回放与审计就不再可信——插件的 bug 会变成「历史无法解释」。

因此把扩展面分三级，按「会不会侵蚀不变量」划线，而不是按「好不好实现」划线：

| 级别 | 扩展点 | 机制 |
|---|---|---|
| 可插（可外部进程） | 工具 | `tools.Source`：builtin / MCP / stdio 插件同一抽象 |
| 可插（外部进程） | LLM 供应商 | 工厂按 `config.type` 构造，v1 switch，v2 注册表 |
| 可插（外部进程） | UI | 协议客户端；Web / 移动端只是另一个 WS 客户端 |
| 可插（外部进程） | 事件消费者 / 审计导出 | 订阅事件流 |
| 可插（仅进程内） | 上下文压缩策略 | `agent.Compactor` 接口，见下 |
| 可插（仅数据） | 权限规则 | `perm.Loader` + 只能收紧的 `TightenRule`，见 ADR-0010 |
| 可插（仅宿主选择） | 执行后端（sandbox / 远程 runner） | `ExecBackend`，工具无权指定，见 ADR-0011 |
| **不可插** | 事件日志语义、任务 / 工具调用状态机、审批流程、权限引擎与放宽决策、存储后端 | — |

## 工具：一个接口吃三件事

```go
type Source interface {
	Name() string
	Tools(ctx context.Context) ([]Tool, error)
	Close() error
}

type Spec struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema，跨语言通用
	Source      string          // builtin | mcp:foo | plugin:bar
	Effects     []Effect        // read | propose_write | process | network
}
```

MCP 支持不是独立特性，而是 `Source` 的一个实现（stdio / http），v2 的第三方插件是第二个实现（stdio JSON-RPC 2.0：`initialize` / `tools/list` / `tools/prepare` / `tools/execute`）。一套抽象避免为 MCP 单独造轮子。

## 宿主中介写盘

工具按 `Effects` 分类，由宿主强制执行，接口层面不留后门：

| Effects | 谁执行 | 审批 |
|---|---|---|
| `read` | 工具，返回数据 | 自动 |
| `propose_write` | 工具只返回 `{path, content}`；**宿主**算 hash / diff、走审批、CAS 落盘 | 必审批 |
| `process` / `network` | 工具自己执行 | 必审批，回报 argv / 目标并审计 |

写盘永远由宿主代劳，插件拿不到「无审批写入」的能力。这是 ADR-0006 在插件场景下的延续：准备阶段算清后果、审批、再执行。

## 信任模型

无容器（ADR-0001）意味着插件进程与 server 同权限：**插件是受信代码，不是沙箱**，文档不得宣称隔离。可承诺的只有三条：副作用被审批门控、全程审计、一插件一进程（崩溃或超时只影响该 Source，`Close()` 负责回收）。审计行必须带 `source`（`builtin` / `mcp:foo` / `plugin:bar`），否则事后无法定位是谁做的。

## 上下文压缩只做策略

压缩是纯派生物：只决定这一轮给模型看什么，失败可重算。因此可以插策略，但宿主保留三条硬约束：

1. **硬预算二次校验**——策略返回后宿主再数一遍，超限回退 `TruncateOldest` 并记 `strategy_fallback`。
2. **保留项不可丢**——system、最近一条 user、触发本轮的工具结果由宿主校验。
3. **结果只缓存不入库成事件**——单独 `compactions(session_id, from_seq, to_seq, strategy, version, summary, created_at)` 表，按 seq 区间复用，避免同一段历史出现两个摘要版本。

压缩器不做成外部插件：几十行的策略外部化后要付进程管理、schema、超时的成本，收益为零；而且外部进程拿不到 `llm.Provider`，摘要类策略根本无法实现。

## 验证强度：A / B / C

「宿主能验证输出」不是一个强度，分三级：

| 级别 | 判据 | 当前例子 | 要求 |
|---|---|---|---|
| A 可计算 | 宿主当场能算出对错并拒绝 | diff + CAS、预算二次校验、feed 签名与版本单调 | 扩展点默认应落在这级 |
| B 可结构 | 能拒绝畸形，不能拒绝语义错误 | tool schema、argv / backend 审计 | 必须在 ADR 里声明影响范围 |
| C 仅可记录 | 当下无法判定，只能事后追 | prompt hash、压缩摘要 | 必须能一键回默认，且不得参与安全判定 |

C 级只影响质量、不影响安全；一旦让它参与安全判定，等级就被虚假提升了。

## 准入清单

新增扩展点必须在 ADR 里逐条回答，答不上就退回：

1. 它位于哪一层，会不会让事件日志或审批语义变得不可信？
2. 宿主用什么验证它的输出，A / B / C 哪一级？
3. 失败时降级到什么？会不会变成运行前提？
4. 它的身份怎么进审计（`source` / `backend` / feed version / `prompt_hash`）？
5. 能不能不改 `agent` / `app` 就接上？
6. 撤销路径是什么（禁用、回默认、删信任记录）？

## 留缝不等于实现

两类成本必须分开：

- **字段缝**：`PreparedCall.Backend`、`Ruleset.Version`、`Segment.Source`、`tasks.extensions_digest`——v1 就留，合计几十行，事后补会返工。
- **加载器与进程协议**：stdin/stdout JSON-RPC、远程拉取、插件进程管理——**一行都不提前写**。`Source` 的形状在见到真实工具能力（MCP）之前定不下来，提前定型大概率要改。

## Considered Options

- **全部插件化（含权限判定与事件语义）** — 生态最好，但回放与审计失效，ADR-0001 的「唯一安全边界」直接作废。
- **用 Go `plugin` 包（.so）** — 免进程管理，但只支持部分平台、构建参数必须完全一致、崩溃带走整个进程，且无法跨语言。
- **编译期注册（blank import）** — 最简单最安全，但每次加工具都要重新构建二进制，第三方无法独立分发。
- **WASM（wazero）沙箱** — 真隔离，但能力模型复杂、启动与 ABI 成本高，v1 用不上。

## Consequences

- v1 只留接口不实现宿主：`Source` / `Spec.Source` / `Effects` / `Compactor` / `Loader`，约几十行，不动 M1 计划。
- 插件宿主（进程管理、schema 校验、超时、错误分类、审计）估 3–5 天，放 v2，与 MCP 同批做最划算。
- 加工具 = 新 `Source` 实现，不改 `agent` / `app`；加供应商 = 新适配器 + 一套映射测试。
- 任何人不能通过插件放宽权限，放宽只能改本地配置（ADR-0010），灵活性换可审计性。
- 对外的扩展承诺有明确边界，避免「插件能改一切」的错误预期。
