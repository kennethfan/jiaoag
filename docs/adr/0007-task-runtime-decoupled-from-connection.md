# 任务运行时与连接解耦，审批以内存 future 挂起

M0 的 WS 连接是长连接。最省事的做法是把任务跑在连接的处理 goroutine 里，审批直接读写同一个 socket；代价是连接一断任务就死，中途关掉 TUI 等于丢掉一轮工作，而 agent 的一轮常常要跑几十秒到几分钟。

因此任务由 `agent.Runtime` 在独立 goroutine 里运行，状态只写数据库；连接只做订阅与命令投递。需要审批时，运行时把 `waiting_approval`、提案、审批行与事件在一个事务里落库，然后阻塞在

```go
select {
case d := <-respCh:   // app.RespondApproval 投递
case <-ctx.Done():   // 取消 / 服务端关闭
}
```

连接断开不影响 channel，重连后按 seq 回放即可看到「正在等待审批」。

## Considered Options

- **任务跑在连接 goroutine 上** — 代码最短，但断连即中断任务，与「断连继续跑」的目标直接冲突。
- **审批等待轮询数据库** — 无需内存 channel，但引入延迟与轮询开销，取消语义也更绕；单进程部署下内存投递更直接。
- **审批改成对客户端的同步 RPC** — 会把任务进度绑在请求生命期上，等于把连接耦合换个名字。

## Consequences

- 内存 channel 不持久：进程重启后 `waiting_approval` 一律标 `expired`、`running` 标 `interrupted`，v1 不自动续跑。恢复策略变更需新 ADR。
- 审批**不设超时**：只能由用户响应或取消任务。避免「人离开几分钟任务就自动失败」这种意外；代价是无人值守时任务会一直挂着。
- 同一任务必须单飞：`Runtime` 保证同一 task 不会被两个 goroutine 同时推进，否则投影与 seq 都会乱。
- 每个任务 goroutine 必须 `recover`，失败落 `failed{internal}` 事件，不能让一个任务的 panic 带崩进程。
