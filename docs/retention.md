# 数据保留

`events` 与 `audit` 是追加式的：会话、任务、审批、工具调用的每一条痕迹都在里面。这对追责有利，但长期运行会让库一直长大。保留策略就是给这件事一个可控的出口。

## 粒度：整个会话

清理的单位是**会话**，不是行：

- 事件是会话回放的唯一来源。单独删掉一段 `events`，那条会话在 TUI / Web 里就会变成残缺历史（任务在、经过没了）。
- 审计与工具调用、审批互相引用。只删 `audit` 会留下「有操作、没记录」的任务。

所以删除走的是 `App.DeleteSession`：一个事务里按序清掉任务、事件、审计、审批、提案、工具调用与会话授权。要么整块没了，要么原样都在。

## 什么会被删

同时满足两个条件才会成为候选：

1. **所有任务都已结束**（没有 `queued` / `running` / `waiting_approval`）；
2. **最后活动时间早于保留期**（`sessions.updated_at`，任何新事件都会刷新它）。

所以「很久没用的旧会话」会被清掉，「昨天建的但一直在用的会话」不会。

## 手动清理

```bash
# 先看会删哪些（不动数据）
jiaoag admin prune -older-than 90d -dry-run

# 确认后删除
jiaoag admin prune -older-than 90d -yes

# 用别的配置（例如备份库）
jiaoag admin prune -older-than 30d -yes -config /backup/config.yaml
```

`-older-than` 接受 `90d`、`720h`、`30m`，或纯数字（按天）。不带 `-dry-run` 也不带 `-yes` 时只打印计划并以退出码 1 结束——避免误删。

清理直接打开数据库，不需要服务端、令牌或 WS。与运行中的服务端同时操作是安全的（SQLite WAL + 写锁），删到一半有任务被提交的会话会被跳过并在 stderr 报出来，其余照删。

## 自动清理

```yaml
storage:
  retention_days: 0   # 0 = 不自动清理（默认）
```

配成正数后，每个运行时会：

- **启动时**跑一次；
- 之后每 **24 小时**跑一次。

日志里能看到：

```
level=INFO msg=保留策略：已删除过期会话 deleted=3 retention_days=90
```

多个运行时同时开着时各自都会跑，删除是幂等的，第二个通常什么也找不到。某个会话刚好被提交了新任务时会删除失败，记一条 `部分过期会话未删除` 的 Info，不当成故障。

## 没被清理的东西

- **输出文件**：`output_dir` 下工具输出的日志不归这里管，需要自己按文件系统策略清理。
- **后台进程记录**：`background_procs` 里 `running` 的进程属于正在干活的会话，不会被删；孤儿记录用 `/ps`（或 Web 的「进程」）手动处理。
- **运行中会话**：即使很旧，只要有未结束任务就不会被删。

## 观测

```sql
-- 每个会话的最后活动与任务数，估算还能留多久
SELECT id, title, updated_at,
       (SELECT count(*) FROM tasks t WHERE t.session_id = sessions.id) AS tasks
FROM sessions ORDER BY updated_at ASC LIMIT 20;

-- 库的大小构成
SELECT 'sessions', count(*) FROM sessions
UNION ALL SELECT 'tasks', count(*) FROM tasks
UNION ALL SELECT 'events', count(*) FROM events
UNION ALL SELECT 'audit', count(*) FROM audit;
```

搭配 `/metrics` 的 `jiaoag_sessions` / `jiaoag_events` / `jiaoag_audit_rows` 可以看趋势。

## 边界

- 删除不可撤销，也没有软删除。要用就先用 `-dry-run` 看清单。
- 保留期只看 `sessions.updated_at`，不看单条事件的时间——一个会话里个别旧事件会跟着会话一起留到最后。
- 不做按行数/按库大小的自动清理：那会以「删到一半的会话」为代价，和上面第一条冲突。