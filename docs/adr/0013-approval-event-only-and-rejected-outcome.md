# 审批结果只走事件流，未落盘的结局统一为 proposal.rejected

M4 落地写入与审批时，《v1 实施计划》里写的是 `approval.respond` 加一组独立的 `approval.requested` / `approval.resolved` 协议帧。实现时发现两条投递路径会带来同一个事实的两个版本：客户端从帧里看到一次裁决，从事件回放里又看到另一次，断线重连后尤其容易分叉。

## Decision

- **只新增 C→S 的 `approval.resolve{session_id, approval_id, decision, note}`**。S→C 不新增审批专用帧：`approval.requested` / `approval.resolved` 本来就是 domain 事件，经由既有的 `type=event` 帧投递。审批 UI 因此只有一个数据源，回放天然一致。`decision` 取 `allow_once | allow_session | deny`。
- **新增事件 `proposal.rejected{proposal_id, path, status, reason}`**，`status` 取 `rejected | stale | failed`。提案没落盘的三种结局不拆成三种事件类型：对回放消费者它们的含义相同——这次写入没有发生；区别写在 `status` 里，界面与审计按需展示。
- **`allow_session` 只在内存里按会话生效**，键是 `sha256(tool + "\x00" + args 原文)`。命中时仍照常记录 `approval.requested` 与 `approval.resolved{source: policy}`：放行也是一次决定。
> **落地说明（M5）**：`tool.result.status` 直接取工具调用的终态（`succeeded` / `failed` / `denied` / `canceled`），不从 `IsError` 反推——「用户拒绝」不是「工具出错」，回放与界面必须能区分。等不到裁决就结束的调用（任务取消、进程重启）也要补一条 `status=canceled`、`is_error=true` 的 `tool.result`：每个 tool_use 恰好一条 tool_result，否则回放出的历史带着没有回填的调用，下一次请求会被供应商直接拒绝。策略拒绝执行「要求审批但没有可审内容」的工具时记 `tool.denied`、`actor=policy`。

- **取消与进程重启的收尾来源是 `system`**：任务取消时用 `context.WithoutCancel` 写 `approval.resolved{expired, source=system}`、把提案转 `stale` 并补一条 `proposal.rejected{stale}`、把工具调用标 `canceled`；启动时 `ExpirePendingApprovals` 对遗留待审批做同样的收尾（审批 / 提案 / 工具调用三份投影一起收，没有协程的任务标 `canceled`）。两条路径必须一致：只改投影不写 `proposal.rejected`，回放就会看到一条永远 `pending` 的提案，这正是本 ADR 要消灭的不一致。
> **落地说明（M5）**：启动清理不止审批。`ReapUnfinished` 还会收尾没有审批行的遗留状态：停在 `requested|prepared|waiting_approval|running` 且没有 `tool.result` 的调用标 `canceled` 并补一条 `tool.result`，仍未终止的任务一律标 `canceled`。崩溃时正在跑只读工具、或刚写完 `tool.prepared` 就断掉的调用没有审批行可查，只清 pending 审批会让它们永远停在半路，历史里也就永久带着没有回填的 `tool_use`。唯一例外是审批已裁决、却缺 `tool.result` 的调用：那次调用是否跑过无从判断，只报告不补写。

## Considered Options

- **独立审批帧 + 事件双写**（原计划）— 客户端实现更直接，但同一事实两条路径，迟早不一致；断线重连还要额外对账。
- **把 rejected / stale / failed 拆成三种事件** — 类型更自解释，但事件类型是长期契约，三种语义高度重叠，拆开只会让消费者多写三个分支。
- **`allow_session` 落库** — 重启后仍有效，但等于把「一次批准」升级成永久豁免，与「每次写入都要有人负责」冲突。

## Consequences

- 客户端必须从事件流驱动审批界面，不能依赖请求-响应式回执；重复的 `approval.resolve` 是幂等空操作。
- 新增事件类型要同步 `domain.EventKind`、TUI 渲染与回放消费者；`proposal.rejected` 的 `status` 是展示字段，不是新的状态机。
- 会话白名单不跨进程、不跨会话；重启后同一个写入会重新询问。这是有意的。
- 审计里 `source` 必须能区分 `user` / `policy` / `system`，否则「谁批准了这次写入」无法回答。
