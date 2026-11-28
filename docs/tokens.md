# 多令牌与只读权限

单个 `server.token` 的问题很直接：它同时是「谁能连」和「谁能改」，泄漏一次等于把工作区和审批权一起交出去。`server.tokens` 把这两件事拆开，并且支持不重启就换。

## 配置

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
- 客户端（TUI / `jiaoag events`）取第一个可写令牌：优先 `server.token`，否则 `tokens` 里第一个 `read_only: false`。只有只读令牌时客户端配置校验会失败，并提示补一个可写令牌。

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

## 热重载与吊销

改完配置发 `SIGHUP`：

```bash
kill -HUP "$(pgrep -f 'jiaoag serve')"
```

- 只重读令牌，监听地址、TLS 证书、并发上限仍需重启。
- 重载失败（YAML 写坏、令牌值为空、集合为空）只记 Error 并**保留旧令牌**，不会把所有人挡在门外。
- 吊销 = 删掉那条令牌再 HUP；从下一次握手开始生效。
- 已建立的连接不因重载断开（令牌在握手时定格在连接上），要立即踢掉就重启服务端或断开那台客户端。

Windows 上没有 `SIGHUP`，`signal.Notify` 收到它不会触发——那边换令牌需要重启服务端。

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
- 令牌一律明文比较的常数时间实现；文件权限仍然是你的责任（`chmod 600`）。
- 只读令牌能看全部会话的事件与审计——它是「观测者」，不是「受限用户」。
- 与 TLS 独立：明文连接下令牌仍会在网络上裸奔，远程使用必须配 `tls_cert`/`tls_key`（见 [`tls.md`](tls.md)）。
- `/healthz` 的匿名响应只说明「进程活着」，不代表数据库可用或任务能跑；要判断可用性请走 WS 握手。
- 只读令牌可以抓 `/metrics`（HTTP 鉴权，不走 WS 的写命令白名单）；监控用只读令牌比用可写令牌安全。
