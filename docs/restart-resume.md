# 重启续跑

服务端重启后，**排队中的任务会继续执行**：不用重新提交，顺序也不变。执行中或等审批的任务不会自动重跑——它们可能已经产生了副作用。

设计动机与取舍见 [ADR-0017](adr/0017-restart-resume.md)。

## 行为

| 重启时的状态 | 重启后 |
|---|---|
| `queued`（还没轮到的） | 保留，按原提交顺序重新入队并执行 |
| `running` | 标 `canceled`；缺 `tool.result` 的调用补一条 canceled |
| `waiting_approval` | 标 `canceled`；遗留审批标 `expired`、提案标 `stale` |
| `completed` / `failed` / `canceled` | 不变 |

恢复后的第一条状态事件仍是 `task.queued`（提交时写的），随后是正常的 `task.status queued → running`。TUI 全量回放即可看到队列，不需要额外协议。

## 配置

```yaml
agent:
  # 默认 true。设为 false 恢复 v1 行为：关闭时把排队任务一并取消。
  resume_queued: true
```

关闭续跑后：

- 正常关闭时，内存队列里的任务写 `canceled{reason: shutdown}`；
- 启动时残留的 `queued` 也会被启动收尾标 `canceled`。

## 恢复流程

1. 启动收尾（`ReapUnfinished`）：收尾未完成的工具调用；`queued` 任务跳过，`running` / `waiting_approval` 标 `canceled`。
2. `resumeQueuedTasks`：读未完成任务，过滤 `queued`，按 `created_at, id` 升序。
3. 逐条重建执行环境（工作区、工具集、供应商）。失败的任务标 `failed{reason: resume_failed}`，不影响其它任务。
4. 按会话入队；每个会话拉起队首。全局并发槽（`max_concurrent_tasks`）仍然生效，抢不到槽的任务留在队首。

恢复时如果扩展指纹（工具集 / 规则集 / 后端 / 系统提示）与提交时冻结的值不同，只记一条 `WARN`，不阻断——任务用当前配置执行，差异留在日志里。

## 查状态

```sql
-- 重启后仍在排队的任务（正常情况下应该很快变成 running）
SELECT id, session_id, status, created_at FROM tasks WHERE status = 'queued' ORDER BY created_at;

-- 恢复失败的任务
SELECT id, session_id, substr(payload, 1, 200)
FROM events
WHERE type = 'task.status' AND json_extract(payload, '$.reason') = 'resume_failed';
```

## 边界

- **不恢复执行中任务**：命令可能已经跑过，文件可能已经改过，自动重跑会重复副作用。需要重做请重新提交，并用回放/审计确认上次做到哪一步。
- **不恢复审批等待**：审批是进程内状态（`allow_session` 白名单也只在内存里），进程退出即失效；重启后需要重新发起审批。
- **at-least-once 的边界**：`queued` 任务在启动前没有任何工具执行，重复拉起是安全的。状态迁移 `queued → running` 在协程内、执行前写入；进程若在这之后崩溃，任务重启时已是 `running`，会被标 `canceled`，不会重跑。
- **恢复会消耗额度**：启动即继续执行，不需要人工确认；不想自动跑就设 `resume_queued: false`。

## 验证

```bash
# 1. 起服务，提交一个会阻塞一会儿的任务，再提交第二个（进入排队）
#    然后在第一个结束前停掉服务（Ctrl-C / SIGTERM）
# 2. 重新启动服务
# 3. 观察第二个任务：应自动从 queued 变为 running，最终 completed

sqlite3 ~/.local/share/jiaoag/jiaoag.db \
  "SELECT id, status, created_at FROM tasks ORDER BY created_at;"
```

单元/集成测试覆盖在 `internal/app/resume_test.go`：

- `TestResumeQueuedTasksAfterRestart`：执行中的任务 canceled、排队任务保留，新进程里跑完；
- `TestResumeQueuedTaskFailureMarksFailed`：工作区已不存在时标 `failed{resume_failed}`，不会永远停在 queued。