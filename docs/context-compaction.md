# 上下文压缩

长任务的上下文会随轮次增长。默认开启的压缩把**较早轮次**交给当前模型摘成一段摘要，摘要作为一条 `context.compacted` 事件留在日志里；最近 2 轮始终原样保留。

设计与取舍见 [ADR-0016](adr/0016-context-compaction.md)。

## 行为

- **触发**：一轮开始前，历史超过 `agent.context_budget_chars` 的 80% 才尝试。
- **切点**：保留最后 2 个用户提问及其之后的内容；同一提问内没有可切点时不压缩。切点落在工具调用边界之前，不会把 `tool_use` 和 `tool_result` 拆开。
- **最小量**：待摘要的旧内容不足 2000 字符时跳过，避免为一点点内容多打一次模型。
- **冷却**：压缩成功后 3 轮内不再尝试。
- **失败**：模型调用失败、超时（90s）或返回空摘要时，只记 `context.compact_failed` 审计，任务继续，历史保持原样。
- **回放**：重建历史时用摘要替换被覆盖的前缀，再接上保留的近期消息。断线重连、重启后看到的上下文与压缩当时一致。

摘要请求本身也是一次模型调用，其输入输出 token 计入任务用量，并在审计里留一份。

## 配置

```yaml
agent:
  context_budget_chars: 60000   # 字符预算
  context_compact: auto         # 默认；off 关闭，只按预算丢弃
```

关闭后回到 v1 行为：超预算时从最旧轮次开始丢弃。摘要提示属于代码，不开放配置。

## 可观测

TUI 会显示一行：

```text
⋯ 上下文已压缩：12 条较早消息 → 843 字摘要
```

事件流里是 `context.compacted`，载荷：

```json
{
  "summary": "…",
  "covered_messages": 12,
  "before_chars": 5210,
  "summary_chars": 843,
  "model": "agnes-3.0-flash",
  "prompt_tokens": 1830,
  "completion_tokens": 260,
  "duration_ms": 2140
}
```

审计：

```sql
SELECT at, action, actor, detail
FROM audit
WHERE action LIKE 'context.compact%'
ORDER BY id DESC;
```

成功是 `context.compact`，失败是 `context.compact_failed`（`detail.error`）。两者 `actor` 都是 `system`。

## 边界

- 摘要是**有损**的。它可能漏掉细节或写错结论；需要精确内容时（行号、完整 diff、报错原文）仍应重新读文件或查日志。
- 压缩不改变事件日志里已有的工具结果与消息；原始事实仍在，摘要只影响喂给模型的历史。
- 压缩是优化，不是运行前提：provider 不可用时任务照常继续，只是上下文压力仍在，后续轮次会继续按预算丢弃。
- 跨会话的项目记忆不在范围内；这里只处理单个任务内的历史。
