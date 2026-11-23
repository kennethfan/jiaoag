# 重启续跑：只恢复排队任务

v1 的启动清理把上一次进程留下的所有非终态任务一律标成 `canceled`。进程正常重启一次，排队里的任务就全没了；而排队任务根本没有开始执行，重拉没有任何副作用，没有理由丢掉它们。本 ADR 把「重启」从「丢队列」改成「继续排队」。

## 背景

任务状态只有 queued / running / waiting_approval / completed / failed / canceled。队列本身只在内存里，数据库里的 `queued` 是唯一事实来源：

- 关闭时 `cancelQueuedOnClose` 把内存队列里的任务逐个写成 `canceled{reason: shutdown}`；
- 启动时 `ReapUnfinished` 再把残留的非终态任务收尾。

这两步对 running / waiting 是必要的：它们可能已经执行过工具，结果无从判断，重跑会重复副作用。但对 queued 就过于保守了——这些任务从未启动，`agent.Run` 一次都没进过，恢复它和用户重新提交一次等价，区别只是保留原来的提交顺序与 prompt。

## 决策

1. **只恢复 `queued`。**
   `running` / `waiting_approval` 仍由 `ReapUnfinished` 标 `canceled`。已裁决审批的调用还会补一条「执行中进程重启，结果未知」的 `tool.result`。不做自动重跑，避免同一个 `write_file` / `run_command` 执行两次。

2. **默认开启，可显式关闭。**
   新增 `agent.resume_queued`（默认 `true`）。设为 `false` 时恢复 v1 行为：关闭时把排队任务标 `canceled`，启动时也不重建队列。

3. **关闭路径保留 `queued`。**
   `close` 在置位 `closed` 时一并读取该开关；开启时跳过 `cancelQueuedOnClose`，内存队列直接丢弃，数据库里仍是 `queued`。

4. **启动收尾跳过 `queued`。**
   `ReapUnfinished` 在开启续跑时对 `queued` 只记日志、不写终态；工具调用收尾不受影响（排队任务不会有工具调用）。

5. **按提交顺序重建每会话 FIFO。**
   `resumeQueuedTasks` 读 `UnfinishedTasks`，过滤 `queued`，按 `created_at, id` 升序；对每条任务取会话、重建 agent（工作区、工具集、供应商），按会话入队，最后每个会话拉起队首。全局并发槽仍由 `startLocked` 自己抢，抢不到就留在队首等 pump。

6. **重建失败标 `failed{reason: resume_failed}`。**
   工作区已被删除、供应商配置失效等情况下，任务不能永远停在 `queued`。失败只影响该任务，其余任务继续恢复。为此状态机新增 `queued → failed` 迁移。

7. **不新增表、事件或协议帧。**
   恢复后的任务沿用原来的 `task.queued` 事件与 `task.status` 迁移；TUI 全量回放时看到的就是「排队 → 运行」，与正常提交无差别。

8. **配置漂移只告警。**
   任务里冻结的 `extensions_digest` 与本次重建算出的不一致时记 `WARN`，不阻断。恢复用的是当前配置的工具集与规则集；差异留在日志里，便于解释「两次启动跑的不是同一套组合」。

## 后果

- 正常重启（部署、升级、手动重启）不再丢排队任务；进程启动后会自动消耗 provider 配额继续跑，直到队列清空。
- 崩溃恢复仍不完整：执行中的任务只能 canceled，审批等待无法恢复。要做「续跑执行中任务」需要持久化更细的中间状态并接受重复副作用，不在本 ADR 范围内。
- `queued → running` 在恢复时由执行协程的 `markRunning` 写入，与正常路径一致；不会出现两条 running。
- 关闭窗口内还没启动的任务：`close` 先取消 baseCtx，再等 running 收尾；队列里的任务保持 `queued`，下次启动恢复。

## 备选方案

- **自动重跑 running / waiting**：会让工具重复执行，尤其是写文件与命令审批通过后的命令。拒绝。
- **持久化内存队列（队列表 / 队列快照）**：数据库里的 `queued` 已经是队列，另建一份只会制造两个事实来源。
- **默认关闭、按需开启**：对单用户本地服务来说，重启丢队列是意外而不是保护；默认开启更符合预期，需要旧行为时一个开关即可。
- **恢复时改写任务指纹**：会抹掉「提交时是哪套扩展」这一事实；审计需要原始值，因此只告警不改写。