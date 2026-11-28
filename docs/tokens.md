# 访问令牌：落库、只读权限、轮换与限速

单个 `server.token` 的问题很直接：它同时是「谁能连」和「谁能改」，泄漏一次等于把工作区和审批权一起交出去。`server.tokens` 把这两件事拆开；v2.61 起令牌**以数据库为准**，轮换与吊销不用改配置文件、也不用重启（Windows 同样）。

## 配置（首次启动的种子）

```yaml
server:
  listen: 127.0.0.1:8787
  # 老写法仍然有效，等价于名为 default 的可写令牌
  # token: file:~/.config/jiaoag/token
  tokens:
    - name: ops
      value: file:~/.config/jiaoag/token
    - name: viewer
      value: $JIAOAG_READONLY_TOKEN
      read_only: true
```

- `name` 只用于排障与日志（`[A-Za-z0-9_.-]`，≤32，不能重复），不会出现在协议里。
- `value` 的写法与 `server.token` 一致：明文 / `$ENV_NAME` / `file:/abs/path`。
- `token` 与 `tokens` 可以同时存在；`token` 视作名为 `default` 的可写令牌。
- 至少要有一个令牌，否则服务端启动时报错。
- **这份配置只在库为空时导入一次**（启动日志会打 `已把配置里的访问令牌导入数据库`）。之后认证只看 `server_tokens` 表；库里有记录后再改配置不会生效，避免「轮换过的旧值在重启后复活」。
- 客户端（TUI / `jiaoag events`）取第一个可写令牌：优先 `server.token`，否则 `tokens` 里第一个 `read_only: false`。轮换后要把新明文写回这里（或用 `-token` 传）。
- 可选的 `server.token_rate_limit`：单令牌每分钟请求上限，默认 600，`0` 表示不限（见下文「限速」）。

## 权限矩阵

| 命令 | 可写令牌 | 只读令牌 |
|---|---|---|
| `client.hello` / `ping` | ✅ | ✅ |
| `session.list` / `event.subscribe` / `event.head` | ✅ | ✅ |
| `audit.tail` | ✅ | ✅ |
| `provider.models` / `mcp.list` / `skill.list` | ✅ | ✅ |
| `session.create` / `session.update` | ✅ | ❌ `forbidden` |
| `task.submit` / `task.cancel` | ✅ | ❌ `forbidden` |
| `approval.resolve` | ✅ | ❌ `forbidden` |
| `project.trust` / `project.revoke` | ✅ | ❌ `forbidden` |

只读判定在消息循环里统一做（`isWriteCommand`），不在各 handler 里分散判断——新增写命令时补一条即可，漏了会被默认放行，所以那里有注释提醒。

## 管理令牌：`jiaoag admin token`

```bash
jiaoag admin token list                      # 名字 / 权限 / 哈希前缀 / 创建 / 轮换 / 状态
jiaoag admin token create viewer -read-only  # 新建只读令牌，明文只显示这一次
jiaoag admin token rotate default            # 换明文，旧值立刻失效
jiaoag admin token revoke viewer -yes        # 吊销（保留记录，便于追责）
```

- 直接打开数据库，不需要服务端在跑、不需要令牌；多运行时同时写是支持的（WAL + 写锁）。
- 库里只存 sha256，**明文只在 create/rotate 的输出里出现一次**，忘了只能再轮换。
- 改完 ≤2 秒服务端自动生效（版本号轮询）；不想等就 `kill -HUP "$(pgrep -f 'jiaoag serve')"` 让它立刻重读。
- 轮换/吊销只影响**新握手**：已建立的连接权限在握手时定格，不会中途变化。要立刻断开旧客户端就重启服务端。
- 全部令牌都吊销时，服务端会清空内存令牌并拒绝一切连接——不会退回配置里的旧值。

## 限速

```yaml
server:
  token_rate_limit: 600   # 次/分钟；0 表示不限
```

- 覆盖 WS 消息与需要令牌的 HTTP 抓取（`/api/info`、`/metrics`）；`/healthz` 匿名探活不受限。
- 令牌桶按秒回填：600/分钟 = 每秒 10 个，短时突发能用掉整桶，之后按速率恢复。
- 超限时 WS 回 `rate_limited` 错误帧，HTTP 回 429 + `Retry-After: 1`。
- 计数在进程内：多运行时各算一份，所以实际允许速率是 `limit × 运行时数`。单机自用足够；要严格全局限速得把桶也落库。

## 排障

服务端启动与每次重载都会打一行：

```text
访问令牌已加载 count=2 read_only=1 names=ops,viewer(只读)
```

重载成功：`令牌已重载 count=2`；失败：`令牌重载失败，继续使用旧令牌 err=...`。客户端握手日志里也有 `token=viewer read_only=true`，能直接看出这条连接用的是哪把钥匙。

## /healthz 的信息分级

`/healthz` 是探活口，**永远返回 200**，不因为没令牌就 401——TUI 拉起服务端、k8s liveness 探针都不该需要凭据。它只按 `Authorization` 决定 body 里有几个字段：

| 请求 | 返回 |
|---|---|
| 匿名 / 错令牌 | `{"status":"ok"}` |
| 有效令牌（含只读） | `status`、`version`、`protocol_version`、`uptime_seconds`、`connections` |

版本号对应已知漏洞，连接数能看出有没有人在用——这类运行态细节在暴露到内网或反代之后就是免费的侦察材料，所以只给持令牌的人。要监控版本，用带令牌的请求：

```bash
curl -s -H "Authorization: Bearer $(cat ~/.config/jiaoag/token)" 127.0.0.1:8787/healthz | jq .
```

## 边界

- 这是**权限分级**，不是多用户：没有 per-user 归属、配额或用例隔离。
- 库里只有哈希，内存里也只保留哈希（认证时把明文哈希一次再常数时间比较）；令牌文件本身的权限仍是你的责任（`chmod 600`）。
- 只读令牌能看全部会话的事件与审计——它是「观测者」，不是「受限用户」。
- 与 TLS 独立：明文连接下令牌仍会在网络上裸奔，远程使用必须配 `tls_cert`/`tls_key`（见 [`tls.md`](tls.md)）。
- 令牌的真相在库里，`jiaoag admin token` 是唯一推荐入口；直接改表也能生效，但要自己算 sha256 并推进 `server_token_meta.version`。
- `/healthz` 的匿名响应只说明「进程活着」，不代表数据库可用或任务能跑；要判断可用性请走 WS 握手。
- 只读令牌可以抓 `/metrics`（HTTP 鉴权，不走 WS 的写命令白名单）；监控用只读令牌比用可写令牌安全。
