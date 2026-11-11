# 工具调用拆成 Prepare 与 Execute 两段

审批必须在副作用发生**之前**完成，但「审批什么」需要在副作用**之后**才能算准——写文件要算 diff 和 hash，跑命令要解析出真实 argv。若把审批放进执行器内部回调，审批就变成运行时约定：任何一处忘记回调，都会静默地先写后问；审计顺序也不再可信。

因此工具契约拆成两段：`Prepare(ctx, args) (PreparedCall, error)` 只读——解析路径、读出现状、算 sha256、算 unified diff、解析 argv 与超时；`Execute(ctx, PreparedCall) (Result, error)` 才写盘或起进程。审批发生在两段之间，`perm.Evaluate` 的输入就是 `PreparedCall`。

## Considered Options

- **执行器内部回调审批** — 实现直接，但「先写后问」在类型上无法排除，且不同工具的审批时机不可能一致。
- **先执行到临时位置再确认** — 对写文件可行（临时文件），对 `run_command` 完全不可行；两套语义会让审计与状态机分叉。
- **审批只凭工具名与原始参数** — 用户看不到 diff 与真实 argv，「批准」等于盲签，ADR-0004 的白名单判定也没有可靠输入。

## Consequences

- `Prepare` 必须是纯只读：读文件、stat、解析参数，不得写盘、不得起进程、不得访问网络。代码评审以此为准，测试用只读工作区断言。
- 写类工具的 `PreparedCall` 必须带 `before_hash` 与 `diff`；`Execute` 前重算 hash，不一致返回 `stale_proposal`（作为工具结果交回模型重新提案，而不是崩溃）。
- 工具只声明 `SideEffect` 与 `Risk`，不决定是否需要审批——那个判断唯一属于 `perm`。
- 参数归一化（例如把相对路径解析为绝对路径）必须在 `Prepare` 内完成一次，`Execute` 不再重新解释参数。
