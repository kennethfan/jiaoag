# 远程访问与 TLS

默认配置只监听 `127.0.0.1`，TUI 也在同一台机器上，明文 HTTP + Bearer token 就够了。
一旦把 `listen` 改成对外地址，明文就意味着**任何能嗅探流量的人都能拿到 token 并接管 agent**——
它能读写工作区、执行命令。这时必须让传输层加密。

两条路，任选其一。

## 一、服务端直接跑 TLS（wss）

在配置里同时给出证书与私钥：

```yaml
server:
  listen: 0.0.0.0:8787
  token: file:~/.config/jiaoag/token
  tls_cert: ~/.config/jiaoag/tls/cert.pem
  tls_key: ~/.config/jiaoag/tls/key.pem
```

只配一个会直接启动失败（`server.tls_cert 与 server.tls_key 必须同时配置`），
证书内容无法解析也会在监听前报错——不会出现「以为加密了、其实是明文」的部署。

启动后 `healthz` 与 `/ws` 都只在 TLS 上提供：

```bash
curl --cacert cert.pem https://127.0.0.1:8787/healthz
```

TUI 连接：

```bash
# 证书由可信 CA 签发（或用系统信任链验证通过）
jiaoag-tui -url wss://my-host:8787/ws

# 自签证书：显式跳过校验
jiaoag-tui -url wss://my-host:8787/ws -insecure
```

如果不传 `-url`，TUI 会按 `server.listen` 推导；配置了证书时自动用 `wss://`。

### 自签证书

内网自用最省事：

```bash
mkdir -p ~/.config/jiaoag/tls && cd ~/.config/jiaoag/tls
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout key.pem -out cert.pem -days 365 \
  -subj "/CN=jiaoag" -addext "subjectAltName=IP:127.0.0.1,DNS:localhost"

chmod 600 key.pem
```

`-addext subjectAltName` 不能省：现代 TLS 校验只看 SAN，不看 CN。要连别的地址就把它加进去。

自签意味着没有第三方背书，`-insecure` 跳过的正是「这证书是不是真的属于那台机器」。
在你能确认网络路径的场景（家里、Tailscale、跳板机）可以接受；放在公网上则应该用
Let's Encrypt 之类签发的证书，别用 `-insecure`。

## 二、反代终结 TLS

已经有 nginx / Caddy 时，把 jiaoag 继续留在回环地址，由反代转发 WebSocket：

```nginx
location /ws {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_read_timeout 3600s;   # 任务跑得久，别让空闲超时切断
}
```

Caddy 更短：

```text
jiaoag.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

TUI 连 `wss://jiaoag.example.com/ws`。这种方式的好处是证书轮换、HSTS、访问日志都在反代上，
jiaoag 本身保持只监听回环。

## 加密了什么、没加密什么

- **加密**：WS 帧（含 prompt、diff、命令输出）与 `Authorization` 头。
- **没加密**：数据库与输出文件（本地磁盘明文）；token 本身仍是唯一的身份凭据——
  TLS 只保证它在传输中不被截获。
- **没改变**：单用户单 token，没有多用户、没有 token 轮换、没有按会话授权。
  要给别人用，先想清楚「拿到 token 等于拿到这台机器的 agent 权限」。

## 边界

- 没有自动证书签发/续期，证书过期由部署方负责。
- 没有客户端证书（mTLS），身份仍是 Bearer token。
- `-insecure` 是客户端参数，不影响服务端；服务端永远提供它配置的证书。
- 服务端关闭时直接断开 WS 连接（不发 close 帧），客户端按断线处理——重连会在服务端
  重新起来后进行。