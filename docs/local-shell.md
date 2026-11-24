# 本机命令（`!`）

在 TUI 里输入以 `!` 开头的内容，会在**客户端进程**里直接执行，输出逐行刷进对话区。

```text
› !git status --short
! git status --short
  │  M internal/tui/tui.go
  │ ?? internal/localshell/
  ↳ 退出码 0（12 ms）
```

它与 agent 的 `run_command` 是两条独立的路：

| | `!命令`（本机） | `run_command`（agent） |
|---|---|---|
| 谁发起 | 人，直接敲 | 模型，经 `tool.requested` |
| 在哪执行 | TUI 进程（客户端机器） | `jiaoag serve`（服务端机器） |
| 审批 | 不需要 | 默认需要（或命中 argv 白名单） |
| 审计 | 不写 | 写 `exec.*` |
| 写白名单 / sandbox | 不适用 | 受 `exec.backend` 约束 |
| 进入模型上下文 | 否 | 是（工具结果回填） |
| 落库 / 回放 | 否 | 是 |

## 行为

- 执行方式：Unix `sh -c '<命令>'`，Windows `cmd /c`，因此管道、重定向、`&&` 都可用。
- 工作目录：TUI 的 workspace（默认启动目录，`-url` 连远端时仍是本地目录）。
- 环境变量：完整继承（含密钥）。这是你自己的 shell，不是 agent 的命令。
- 输出：stdout 与 stderr 合并，按行流式显示，缩进 `  │ `，按 72 列折行；单行超过 2000 rune 裁剪并以 `…` 结尾。
- 上限：一次最多渲染 400 行，多出的行只计数，结束行显示「省略 N 行」。事件仍会被读完，避免子进程阻塞在写管道上。
- 超时：默认 180 秒，到点整组杀掉，结束行显示「超时 …，已终止」。
- 终止：`ctrl+x` 杀整个进程组（`go test` 派生的编译子进程一起收掉）。
- 并发：同时只允许一条本地命令；它与 agent 任务互不影响（一个在客户端，一个在服务端）。
- 会话无关：不需要 `session.create` 成功，服务端断开时照样能用。

## 不持久化

本地命令不产生事件、不写库，所以：

- 断线重连触发**全量重放**、或收到 `reset` 快照清空视图后，之前的本地输出会消失。
- 重启 TUI 后看不到历史本地命令。
- 模型完全不知道你跑过什么；要让它参与推理，请把结论贴进提问。

## 与其他能力的关系

- 想在**服务端**机器上执行、并留下审计与输出落盘 → 用 agent 的 `run_command`（可命中 argv 白名单免审）。
- 想要进程级隔离（只能写工作区、读不到 `~/.ssh`）→ 那是 agent 命令的 `exec.backend: sandbox`，见 [`sandbox.md`](sandbox.md)；`!` 不受它约束。
- 想让跨会话共享一条长期事实 → 用 `remember` 工具写 `MEMORY.md`，见 [`memory.md`](memory.md)。

## 平台差异

Windows 上 `ctrl+x` 会终止整个进程树：`cmd.exe` 启动后立刻被放进 Job Object，`Stop`/超时时 `TerminateJobObject` 收掉整棵树，再用 `taskkill /T` 兜底覆盖「已派生、还没赋进 Job」的微秒级窗口。建不出 Job 时退化为只 `taskkill /T`，命令本身不会因此失败。

刻意没有设置 `KILL_ON_JOB_CLOSE`：正常结束时关掉句柄不会连带杀掉命令留下的后台进程（`!npm run dev &` 与 Unix 行为一致），只有 `Stop` 与超时才动手。

决策背景见 [`docs/adr/0022-local-shell-command.md`](adr/0022-local-shell-command.md)。