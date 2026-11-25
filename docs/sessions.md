# 会话管理

一个工作区可以有很多会话：每次不带 `-c` / `-s` 启动 TUI 都会新建一条，同一条会话可以隔几天再用 `-c` 接着聊。`/sessions` 是管理这些会话的唯一界面。

## 打开

在 TUI 里输入 `/sessions`（不带参数）：

```text
⇄ 会话（3 条，工作区 ~/code/api）
────────────────────────────────────────
▶ 修登录超时  ses_60cffc40  8 任务  11-24 12:01
      agnes/agnes-3.0-flash · 12.4k/0.6k tok
  重构中间件  ses_c8bcb80b  3 任务  2 进行中  11-24 11:47
      openai/gpt-5 · 3.1k/0.2k tok
  (未命名)  ses_1d8e3311  0 任务  11-23 22:10
[↑↓/jk] 选择  [enter] 切换  [t] 改名  [d] 删除  [esc] 关闭
```

- 只列**当前工作区**的会话。跨目录的会话不会混进来：切过去以后工作区对不上，工具路径会全错。
- 按最近活动（`sessions.updated_at`）倒序，所以「刚才那条」永远在第一屏。
- 每条两行：标题（或 `(未命名)`）+ 短 id + 任务数（有未结束任务时标出「N 进行中」）+ 最后活动时间；第二行是模型与 token。
- 时间只区分新旧：当天显示时刻，更早显示月-日。

## 切换

`enter` 就地切到选中会话，不需要退出重开：

1. 向服务端发 `event.unsubscribe` 停掉旧会话的推送；
2. 清空本地视图，换上新的 session/provider/model；
3. 发 `event.subscribe from_seq=0`，把这条会话的历史整体回放。

第 1 步不能省：服务端的订阅是按会话 id 分键的，不退订就会两条流同时往一条连接上推，事件互相串台。

有未结束任务（`queued`/`running`/`waiting_approval`）的会话不允许切走——审批面板和 `esc` 取消都绑在当前任务上，切走以后它们就没有归属了。想切就等任务结束，或者先按 `esc` 取消。

## 改名

`t` 把当前标题填进输入行，改成想要的再回车保存；直接回车（空标题）表示清掉标题，`esc` 取消。

改名写一条 `session.renamed` 事件，所以重连回放时标题会回来——`session.created` 里存的是创建时的值，之后可能已经改过。标题上限 60 个字符：它只进状态栏与面板，太长两边都会被挤爆。

## 删除

`d` 第一次只提醒，同一行再按一次才真删：

```text
再按一次 d 永久删除 ses_c8bcb80b（含任务、事件与审计，无法撤销）
```

删除是一个事务，按序清掉：

| 表 | 说明 |
|---|---|
| `audit` | 按 `tool_call_id` 反查归属 |
| `approvals` / `proposals` | 按 `task_id` 归属 |
| `tool_calls` | 按 `session_id` |
| `session_approvals` | `allow_session` 的授权记录 |
| `tasks` / `events` | 有外键级联，仍显式删以固定顺序 |
| `sessions` | 最后删，0 行即 `not found` |

只有 `tasks` 与 `events` 声明了 `ON DELETE CASCADE`，其余四张表如果不显式删，会留下指向不存在会话的孤儿行——既没法追责也没法清理，比删掉更糟。

**已知缺口**：审计没有 `session_id` 列，只能经工具调用反查归属。`context.compact` 这类没有工具调用的审计本来就无法归属到会话，删除后仍会留在库里。要留档请先用 `jiaoag events` 导出。

有未结束任务的会话不允许删除（服务端返回 `bad_request`，文案含「还有 N 个未结束任务」）：删掉以后正在跑的 agent 就没地方写事件了。

**删除当前会话**时，界面会自动新建一条：不能让输入行的 `session_id` 指向一个已经不存在的会话。

## 命令行

`/sessions` 要在某个会话里才打不开——命令行工具没有会话上下文，所以它给出两种取工作区的方式：

```bash
jiaoag sessions list                    # 按当前目录筛（先绝对化再比对）
jiaoag sessions list -workspace ~/code/api
jiaoag sessions list -all               # 跨工作区全列（列表始终带工作区列）
jiaoag sessions list -format json       # 给脚本用
jiaoag sessions rename ses_8d64 修登录超时
jiaoag sessions rm ses_8d64 -yes
```

- **id 可以只给前缀**（`ses_8d64`）。列表里的短 id 只显示 12 位，手抄完整 16 位没有意义；前缀命中多条会报错并列出候选。
- **删除必须显式 `-yes`**，否则只打印提醒并退出 1。`-yes` 写在 id 前后都行。
- **走 WS 协议，不直接读 SQLite**：改名要落 `session.renamed` 事件、删除要受「有未结束任务不许删」保护，这两件事只有服务端知道。本地连不上时它会自动拉起 `jiaoag serve`（命令结束后服务端按 30 秒空闲退出），加 `-no-spawn` 可关掉，`-server-bin` 可换可执行文件；已有实例则直接复用。
- 通用标志 `-config` / `-url` / `-token` / `-insecure` 与 TUI、`jiaoag events` 同义，写在子命令前后都行；子命令自己的标志要写在位置参数**前面**（`rm -yes` 例外，任意位置都认）。
- 路径比对会解析符号链接：`/tmp/x` 与 `/private/tmp/x` 是同一个目录，不会因为写法不同就查不到。

服务端对 `session.manage` 的三种入参：带 `session_id` 时用该会话的工作区（TUI 面板）；`all` 跨工作区；否则用请求里的 `workspace`（会做绝对化）。命令行没有会话，只能走后两种——这需要有效令牌，而列表只回元数据、不含事件内容。

## 只读令牌

`session.manage` 是读命令，只读令牌可以看列表；`session.rename` / `session.delete` 是写命令，只读令牌会被挡在 `forbidden`（见 `docs/tokens.md`）。

## 观测

```bash
# 每个工作区有多少会话、多少空会话
sqlite3 ~/.local/share/jiaoag/jiaoag.db "
SELECT workspace, count(*) AS sessions,
       sum((SELECT count(*) FROM tasks t WHERE t.session_id=s.id)) AS tasks
FROM sessions s GROUP BY workspace;"

# 空会话（启动过但没提交任务）
sqlite3 ~/.local/share/jiaoag/jiaoag.db "
SELECT id, datetime(created_at) FROM sessions s
WHERE NOT EXISTS (SELECT 1 FROM tasks t WHERE t.session_id = s.id);"
```

空会话不自动清理：删会话连带审计一起删，那是人的决定，不该由启动逻辑替你做。

## 边界

- **删除不撤磁盘改动**：会话记录没了，它写过的文件还在（`git status` 才是事实）。
- **删除不能撤销**，也没有回收站。
- **切换会话不换工作区**：`-root` 挂的附加根是启动参数，切换后仍是同一组。
- **标题不参与检索**：`/sessions` 只按时间排序，没有搜索框（单用户、几十条量级，够用）。
