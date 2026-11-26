# 架构 v3：契约、传输与运行时所有权

v3 只改「进程与边界」，不改三条不变量：事件日志的语义、状态机与审批流、权限引擎只收紧。业务代码（agent / app / store / perm / exec / tools）一行没动。

## 1. 三个平面

```text
┌──────────────── 客户端 ────────────────┐
│ TUI（内嵌）        TUI（客户端）   Web  │
│   │ localTransport     │ WS        │ WS │
└───┼───────────────────┼───────────┼────┘
    ▼                   ▼           ▼
┌────────── internal/rpc（契约）─────────┐
│ Service（16 个强类型方法）             │
│ Streams（回放→快照→去重→live）         │
│ Principal / Error{Code,Message}        │
└──────────────────┬─────────────────────┘
                   ▼
        app / agent / store / perm / exec
```

- **契约**：`internal/rpc`。只有它知道「一个操作要做哪些校验、返回什么错误码」。
- **传输**：WebSocket（`internal/server/adapters.go`）与进程内（`internal/tui/local.go`）。两者都不含业务判断，只做编码/派发。
- **运行时**：`internal/boot`。谁持有 store 与 agent，谁就是运行时；用文件锁保证只有一个。

## 2. 两种运行形态

| 形态 | 触发 | 端口 | 令牌 | 事件流 |
|---|---|---|---|---|
| embedded | 裸跑 `jiaoag`，拿到数据目录锁 | 无 | 不需要 | `rpc.Streams` 直接写 channel |
| served | `jiaoag serve`，或 TUI 拿不到锁但端口可达 | 有 | 有 | `rpc.Streams` → WS 帧 |

TUI 的选择逻辑：

```text
url 指定 ────────────────► WS 客户端
否则 try lock
  ├─ 成功 ───────────────► boot.Open + localTransport
  └─ ErrLocked
       ├─ 端口可达 ──────► WS 客户端（连已有 serve）
       └─ 否则 ──────────► 报错退出
```

`-no-spawn` 的含义随之变化：**不内嵌、也不拉起**，只连已有 serve。

## 3. 契约的边界

- `Service` 的每个方法都接收 `Principal`；写操作先过 `guardWrite`，只读令牌得到 `forbidden`。这层校验从 server 移到了契约，两个传输天然一致。
- session id 的「缺省取信封上的」属于传输层适配，因为 envelope 是协议细节；契约要求显式传入。
- 错误码只在这里定义；WS 把它编码成 `error` 帧，进程内直接返回 `*rpc.Error`，TUI 用同一套文案展示。

## 4. 事件流

`Streams.Subscribe` 是唯一实现：

1. 订阅 hub（先订阅，避免回放窗口里漏事件）
2. `app.Replay` 分批回放（500 条/批）
3. `from_seq > 0` 时补一帧 `event.snapshot`（head 与待审批）
4. 回放期间缓冲按 seq 去重
5. live 推送；积压溢出时给错误帧，提示带 `from_seq` 重连

进程内模式没有网络缓冲，因此 channel 容量显式设为 256；溢出语义与 WS 一致。

## 5. 生命周期

- 锁：`<db 目录>/runtime.lock`，flock(`LOCK_EX|LOCK_NB`)，进程退出自动释放。
- `boot.Stack.Close()` 幂等，顺序：通知 → 插件 → MCP → app（含后台进程与 store）→ 锁。
- 退出确认：有任务或后台进程时二次确认；确认后取消任务、停后台进程，再关运行时。

## 6. 未纳入本版

- 只读 CLI 直连 SQLite（当前仍走 serve）。
- UDS / 多客户端本地入口（明确不做）。
- 契约的版本化（协议号仍是 v1，帧格式未变）。
