# 事件与审计导出

`jiaoag events` 是一个**只读**消费者：连上服务端，把一个会话的事件流和全库审计增量打到 stdout。它没有写入口，也不改任何状态。

## 用来做什么

- **跟着看**：任务在跑时实时看每一轮的工具调用、审批与状态迁移。
- **导出归档**：把一段时间的审计与事件落成 JSONL，交给日志系统或留档。
- **接自动化**：CI 里跑完一轮后把审计打到构建日志；或定时导出到对象存储。

## 用法

### 直接读库（默认，不需要服务端）

不给 `-url` 时 `jiaoag events` 用只读方式打开 `storage.db_path`（`-db` 可覆盖），不需要服务端、不需要令牌：

```bash
jiaoag events -s ses_xxx -from 0 --until-now > events.jsonl
jiaoag events -db /backup/jiaoag.db -s ses_xxx --until-now -format text
```

实时部分用 250ms 轮询 `events` / `audit` 表实现（与跨运行时订阅同一节奏）。库不存在、未初始化或结构版本落后时会直接报错并提示先启动一次 `jiaoag`——只读命令不会替你建库或迁移。只有显式给 `-url` 时才走 WS（见下文）。

```bash
# 跟最近的会话 tail（Ctrl+C 退出）；默认只看新事件、新审计
./bin/jiaoag events -config ~/.config/jiaoag/config.yaml

# 只看某个会话
./bin/jiaoag events -session ses_xxx

# 常驻采集：持续跟随写文件，日志轮转后自动重开，断线自动重连
./bin/jiaoag events --follow -output ~/var/jiaoag/events.jsonl

# 只关心执行与写入的审计，以及审批与工具事件
./bin/jiaoag events --follow -action exec -action write -type approval -type tool

# 导出历史：从最开始，导出到启动时刻为止后退出
./bin/jiaoag events -session ses_xxx -from 0 -audit-from 0 -until-now > events.jsonl

# 人类可读（长载荷截断，字段不承诺稳定）
./bin/jiaoag events -session ses_xxx -from 0 -format text

# 远端 / 自签证书（token 仍从配置里解析，配置里可以写 file: 路径）
./bin/jiaoag events -url wss://host:8788/ws -insecure
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
| `-until-now` | `false` | 追平启动时刻的水位后退出（适合定时导出），与 `-follow` 互斥 |
| `-follow` | `false` | 持续跟随：断线退避重连并按游标续传，`-output` 文件被轮转后自动重开 |
| `-output` | stdout | 写入文件；`-follow` 时追加，否则覆盖 |
| `-action` | 全部 | 审计 action 前缀过滤，可重复（OR） |
| `-type` | 全部 | 事件类型前缀过滤，可重复（OR） |
| `-interval` | `1s` | 审计轮询间隔（最小 100ms） |
| `-insecure` | `false` | 跳过 TLS 证书校验（仅自签证书） |

### 常驻采集（`--follow`）

`--follow` 是给「跑一整天的采集进程」用的，比不带它多三件事：

- **写文件 + 轮转**：`-output` 指向的文件被 `logrotate`/`newsyslog` 改名后，下一行写入前会检测 inode 变化并重开新文件，不会继续写旧句柄丢掉日志。
- **断线重连**：WS 模式下服务端重启（或网络抖动）时按 500ms → 1s → 2s → 5s 退避重连，带上已经导出的 `seq` / 审计 `id` 续传，不重复也不漏。
- **不因 `--until-now` 退出**：两者互斥，`--follow` 只由 Ctrl+C / SIGTERM 结束。

```bash
jiaoag events --follow -output /var/log/jiaoag/events.jsonl -action exec -action write
```

直连模式下 `--follow` 同样有效（250ms 轮询库），只是没有「重连」这一步——库不在就报错退出。

## 输出

默认 JSONL，一行一个对象，`kind` 区分两类记录：

```json
{"kind":"event","ts":1795434241386,"session_id":"ses_x","seq":42,"task_id":"tsk_x","type":"tool.result","payload":{"tool":"read_file","ok":true}}
{"kind":"audit","id":17,"at":1795434242000,"actor":"user","action":"exec.approved","task_id":"tsk_x","session_id":"ses_x","tool_call_id":"tc_1","detail":{"argv":["go","test"],"cwd":"/repo"}}
```

- **事件**：`seq` 是会话内序号（流式增量 `assistant.delta` 是 `seq=0` 的临时帧，不进日志但会实时出现）；`type` 与 `payload` 对应 `docs/design/agent-architecture.md` 里的事件表。
- **审计**：`id` 全局单调，`action` 见 `docs/adr/0006` 与各功能文档（`exec.*`、`external.*`、`write.applied` / `write.auto` / `write.failed`、`tool.denied`、`context.compact*`）；`task_id` / `session_id` 由工具调用反查，**没有工具调用的审计这两项为空**（例如 `context.compact`）。
- `write.applied` 只在磁盘真的被改之后写一条，带 `path` / `base_hash` / `new_hash` / `bytes` / `decision`，是回答「谁在什么时候改了哪个文件」的直接依据；只应用了部分块时还带 `hunks` / `total_hunks` / `partial: true`，所以「批了哪几块」也能只靠审计回答。`write.failed` 记录落盘失败（例如权限、CAS 之后写文件报错）及原因。被拒绝的写入仍记 `tool.denied`，两条语义不重叠。`write.auto` 是工作区免审写入（ADR-0042）：`actor=policy`，detail 带 `auto: true` 与 `mode`（`workspace` / `tmp`），没有 `decision` 字段——没人裁决过；要单独统计「哪些改动没人看过」就查这一条。

常用过滤：

```bash
# 只看审计
./bin/jiaoag events -session ses_x -from 999999999 -audit-from 0 | jq -c 'select(.kind=="audit")'

# 只看命令批准
./bin/jiaoag events -session ses_x | jq -c 'select(.action? and (.action|startswith("exec.")))'
```

## 指标（`/metrics`）

服务端在同端口暴露 Prometheus 文本格式的指标，**需要令牌**（与 `/api/info` 同级；库的行数与连接数属于侦察材料）。手写 exposition，不引 prometheus 客户端库。

```bash
curl -s -H "Authorization: Bearer $(cat ~/.config/jiaoag/token)" \
  http://127.0.0.1:8788/metrics | head -20
```

Prometheus 抓取：

```yaml
scrape_configs:
  - job_name: jiaoag
    scheme: http
    authorization:
      credentials_file: /etc/jiaoag/token
    static_configs:
      - targets: ["127.0.0.1:8788"]
```

Vector / 其他采集器同理：`Authorization: Bearer <token>`，路径 `/metrics`。配了 TLS 就把 scheme 换成 `https`。

| 指标 | 类型 | 含义 |
|---|---|---|
| `jiaoag_info{version,protocol}` | gauge | 构建版本与协议版本，值恒为 1 |
| `jiaoag_uptime_seconds` | gauge | 服务端启动至今秒数 |
| `jiaoag_goroutines` | gauge | 当前 goroutine 数 |
| `jiaoag_connections` | gauge | 当前 WebSocket 连接数（HTTP 抓取不算） |
| `jiaoag_subscriptions` | gauge | 活跃事件订阅数，跨连接累加 |
| `jiaoag_memory_alloc_bytes` / `_sys_bytes` | gauge | 堆在用 / 从系统申请 |
| `jiaoag_gc_cycles` | gauge | 已完成 GC 次数 |
| `jiaoag_sessions` | gauge | 会话总数 |
| `jiaoag_tasks{status}` | gauge | 按状态（queued/running/waiting_approval/completed/failed/canceled）计数 |
| `jiaoag_events` / `jiaoag_audit_rows` | gauge | 事件行 / 审计行总数 |
| `jiaoag_background_procs{status}` | gauge | `running` 与 `orphaned` 的后台进程数 |
| `jiaoag_metrics_error` | gauge | 库聚合失败时为 1（此时进程指标仍然完整） |

几点取舍：

- **每次抓取都查库**：`COUNT(*)` 在几千到几万行上都是毫秒级，配 15 秒一次的常规抓取没有压力；不引入常驻缓存，避免指标与库不一致。
- **不暴露提示词、路径、任务内容**：指标只回答「有多少」，不回答「是什么」。
- **库读失败不 500**：返回 200 并把 `jiaoag_metrics_error` 置 1，让 scrape 的 `up` 仍然为真、进程指标不丢。
- 只读令牌同样能抓（它是 HTTP 鉴权，不走 WS 的写命令白名单）。

## 语义与边界

- **只读**：没有删除、修订、触发重放的入口；协议侧只有 `event.head`、`event.subscribe`、`audit.tail` 三个读命令。
- **事件是单会话的**：seq 按会话分配，所以一个进程跟一个会话；同时看多个会话就起多个进程。审计 id 是全局的，所以审计那一路其实不限会话（`-from` 只影响事件）。
- **审计靠轮询**：默认 1 秒一轮，延迟下限就是它；事件是推送的，几乎实时。
- **断开不补**：进程退出后不记录游标，下次要么显式给 `-from` / `-audit-from`（可配合 shell 变量存上次的 `next_id`），要么重新从头导出。
- **`-until-now` 是快照**：两个 head 在启动时取定，追平即退；导出期间新产生的事件不在这一份里。
- **不替代 TUI**：这里没有审批能力，看到 `approval.requested` 也不能裁决——那是 TUI（或多开一个 TUI）的事。