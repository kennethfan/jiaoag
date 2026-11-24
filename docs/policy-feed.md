# 签名规则 feed

`policy.d/*.yaml` 是自己写的收紧规则。签名 feed 补的是另一类：**外部持续更新的
威胁数据**——某个程序的哪个 flag 能绕过审批、某个包最近有没有被投毒。这类事实
推不出来，只能订阅。

关键前提：**feed 只能收紧**。它可以让一条本来免审批的命令变成需要审批，但永远
不能放行任何东西。所以即使来源被攻破，后果上限是「多按几次 y」。

## 配置

```yaml
policy:
  dir: ~/.config/jiaoag/policy.d
  feed:
    url: https://example.com/jiaoag/feed.yaml
    keys:
      - "MCowBQYDK2VwAyEA..."      # ed25519 公钥（base64），可配多把轮换
```

- `url` 支持 `https://` 与 `file://`（离线分发、内网共享目录、CI 产物）。
- 签名固定取同址加 `.sig` 后缀：`feed.yaml` → `feed.yaml.sig`。
- `url` 与 `keys` 必须同时配置；只配一个启动即报错。

## 同步

```bash
./bin/jiaoag-policy sync          # 读配置里的 policy.feed
./bin/jiaoag-policy sync -url file:///srv/policy/feed.yaml
./bin/jiaoag-policy -verify       # 离线校验已安装的 feed，不联网
```

流程：下载 → 验签 → 检查有效期 → 挡回滚 → 原子写入 `policy.d/`。任何一步失败都
不会改动已安装的文件。

输出示例：

```text
已更新：version=2026-11-24.1 签发=2026-11-24T02:00:00Z 过期=2026-12-24T02:00:00Z sha256=9f2c1ab0d3e4
  → /Users/me/.config/jiaoag/policy.d/feed.yaml
服务端下次启动会加载这份规则；已在运行的服务端需要重启才生效。
```

重复同步同一份内容会打印「已是最新」，不会写盘。

## feed 文件长什么样

```yaml
version: 2026-11-24.1
issued_at: 2026-11-24T02:00:00Z
expires_at: 2026-12-24T02:00:00Z
rules:
  - id: feed.npm-audit-fix
    programs: [npm]
    subcommands: [audit]
    reason: 该子命令会执行项目里的钩子脚本
  - id: feed.tar-extract
    programs: [tar]
    deny_flags: [-I, --use-compress-program]
    reason: 通过外部程序解包等于任意执行
```

`version`、`issued_at`、`expires_at` 都在签名范围内。`issued_at` 用于回滚检测，
`expires_at` 用于强制更新。

## 回滚与过期

- **回滚**：`policy.d/feed.state` 记住上一次成功安装的签发时间与内容摘要。新 feed
  的 `issued_at` 必须严格晚于它；内容没变则跳过。比版本号字符串可靠——版本命名
  是发布方的自由。
- **过期**：`expires_at` 之后 `sync` 拒绝安装，服务端启动也拒绝加载。理由同上：
  让人看见「规则过期」，而不是以为还在保护。
- `-force` 可绕过这两项（比如发布方误发了未来时间戳，或离线环境需要临时安装
  过期数据）。

## 发布端

```bash
# 一次性生成密钥
openssl genpkey -algorithm ed25519 -out feed.key
openssl pkey -in feed.key -pubout -outform DER | tail -c 32 | base64   # 填进 keys

# 每次发布
openssl pkeyutl -sign -inkey feed.key -rawin -in feed.yaml -out feed.yaml.sig.raw
base64 < feed.yaml.sig.raw > feed.yaml.sig
```

（用 Go 也可以：`ed25519.Sign(private, data)`，再 `base64` 编码。）

## 生效与观测

- 规则集版本会拼成 `builtin-2+feed@2026-11-24.1`，启动日志的 `policy=` 与
  `policy_hash=` 直接反映 feed 是否加载。
- 每条判定的审计 `detail.policy_version` 记录当时的版本，所以「某次为什么要求
  审批」可以追到具体是哪份 feed 的哪条规则（`rule_id`）。
- 规则只会让命令落到审批，不会放行；`rule_id` 形如 `feed.*` 便于区分来源。

## 边界

- 服务端不主动出网：不跑 `sync` 就不会更新。
- 没有增量下发、没有撤销列表——每次是整份文件。
- 公钥轮换要人工：新旧公钥同时放一段时间，换完再删旧的。
- 过期即拒绝启动是刻意的硬失败；长期离线环境请删除 feed 配置或定期重新 sync。