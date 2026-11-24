# 事件与审计导出

`jiaoag-events` 是一个**只读**消费者：连上服务端，把一个会话的事件流和全库审计增量打到 stdout。它没有写入口，也不改任何状态。

## 用来做什么

- **跟着看**：任务在跑时实时看每一轮的工具调用、审批与状态迁移。
- **导出归档**：把一段时间的审计与事件落成 JSONL，交给日志系统或留档。
- **接自动化**：CI 里跑完一轮后把审计打到构建日志；或定时导出到对象存储。

## 用法

```bash
# 跟最近的会话 tail（Ctrl+C 退出）；默认只看新事件、新审计
./bin/jiaoag-events -config ~/.config/jiaoag/config.yaml

# 只看某个会话
./bin/jiaoag-events -session ses_xxx

# 导出历史：从最开始，导出到启动时刻为止后退出
./bin/jiaoag-events -session ses_xxx -from 0 -audit-from 0 -until-now > events.jsonl

# 人类可读（长载荷截断，字段不承诺稳定）
./bin/jiaoag-events -session ses_xxx -from 0 -format text

# 远端 / 自签证书（token 仍从配置里解析，配置里可以写 file: 路径）
./bin/jiaoag-events -url wss://host:8788/ws -insecure
```

| 参数 | 默认 | 说明 |
|---|---|---|
| `-config` | `~/.config/jiaoag/config.yaml` | 用于解析 `server.listen`、`server.token` 与 TLS 开关 |
| `-url` | 由 `server.listen` 推导 | 覆盖地址；配了证书时推导成 `wss://` |
| `-token` | 配置里的 `server.token` | 覆盖 token |
| `-session` | 最近活跃会话 | 会话 id |
| `-from` | `-1` | 事件起始 seq；`-1` 只看新事件，`0` 导出全部历史 |
| `-audit-from` | `-1` | 审计起始 id；同上 |
| `-format` | `json` | `json`（每行一条）\| `text` |
| `-until-now` | `false` | 追平启动时刻的水位后退出（适合定时导出） |
| `-interval` | `1s` | 审计轮询间隔（最小 100ms） |
| `-insecure` | `false` | 跳过 TLS 证书校验（仅自签证书） |

## 输出

默认 JSONL，一行一个对象，`kind` 区分两类记录：

```json
{"kind":"event","ts":1795434241386,"session_id":"ses_x","seq":42,"task_id":"tsk_x","type":"tool.result","payload":{"tool":"read_file","ok":true}}
{"kind":"audit","id":17,"at":1795434242000,"actor":"user","action":"exec.approved","task_id":"tsk_x","session_id":"ses_x","tool_call_id":"tc_1","detail":{"argv":["go","test"],"cwd":"/repo"}}
```

- **事件**：`seq` 是会话内序号（流式增量 `assistant.delta` 是 `seq=0` 的临时帧，不进日志但会实时出现）；`type` 与 `payload` 对应 `docs/design/agent-architecture.md` 里的事件表。
- **审计**：`id` 全局单调，`action` 见 `docs/adr/0006` 与各功能文档（`exec.*`、`external.*`、`tool.denied`、`context.compact*`）；`task_id` / `session_id` 由工具调用反查，**没有工具调用的审计这两项为空**（例如 `context.compact`）。

常用过滤：

```bash
# 只看审计
./bin/jiaoag-events -session ses_x -from 999999999 -audit-from 0 | jq -c 'select(.kind=="audit")'

# 只看命令批准
./bin/jiaoag-events -session ses_x | jq -c 'select(.action? and (.action|startswith("exec.")))'
```

## 语义与边界

- **只读**：没有删除、修订、触发重放的入口；协议侧只有 `event.head`、`event.subscribe`、`audit.tail` 三个读命令。
- **事件是单会话的**：seq 按会话分配，所以一个进程跟一个会话；同时看多个会话就起多个进程。审计 id 是全局的，所以审计那一路其实不限会话（`-from` 只影响事件）。
- **审计靠轮询**：默认 1 秒一轮，延迟下限就是它；事件是推送的，几乎实时。
- **断开不补**：进程退出后不记录游标，下次要么显式给 `-from` / `-audit-from`（可配合 shell 变量存上次的 `next_id`），要么重新从头导出。
- **`-until-now` 是快照**：两个 head 在启动时取定，追平即退；导出期间新产生的事件不在这一份里。
- **不替代 TUI**：这里没有审批能力，看到 `approval.requested` 也不能裁决——那是 TUI（或多开一个 TUI）的事。