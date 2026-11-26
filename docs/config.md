# 配置命令

配置默认在 `~/.config/jiaoag/config.yaml`，首次用 `jiaoag serve -init` 生成样例。
改了配置后需要重启 `jiaoag serve` 才生效；`jiaoag config check` 可以不启动就验证。

## 子命令

```
jiaoag config path       # 生效的配置文件路径（~ 已展开）
jiaoag config show       # 生效值 + 来源，密钥脱敏
jiaoag config check      # 离线自检；--online 探测供应商/MCP/TLS
jiaoag config set …      # 改一个字段：默认只打印 diff
jiaoag config edit       # $EDITOR 打开，保存后自动校验
```

### show

```
$ jiaoag config show
server.listen                 127.0.0.1:8788                    # config.yaml:6
server.token                  file:~/.config/jiaoag/token（32 字节） # config.yaml:8
agent.max_rounds              0                                 # config.yaml:37
agent.write_approval          workspace                         # 默认值
providers[0].api_keys         8 把                              # config.yaml:64
```

来源只有三种：`config.yaml:<行号>`、`默认值`、`$ENV_NAME`（密钥类）。密钥只报来源与解析后的长度，
`config show` 本身不会把明文打到终端或日志里。

`-format json` 输出 `{"file": "...", "fields": [{"path","value","source"}]}`，便于脚本与 `jq`。

### check

```
$ jiaoag config check
✓ 配置可用

$ jiaoag config check -role client     # TUI/events/sessions 这类客户端配置
$ jiaoag config check -online          # 额外探测供应商 /models、MCP 启动、TLS 握手
```

离线部分检查：YAML 语法、按角色跑 `ValidateServer` / `ValidateClient`、逐个令牌能否解析、
`storage.db_path` 与 `storage.output_dir` 是否可写。警告（不阻止启动）：监听非回环却未配 TLS、
`agent.write_approval: off`。

`--online` 会真的出网与起子进程，默认关。

### set

```
$ jiaoag config set agent.max_rounds 5
--- a/config.yaml
+++ b/config.yaml
@@ -34,7 +34,7 @@
 agent:
-  max_rounds: 0
+  max_rounds: 5
（未写入；确认后加 -y 应用）

$ jiaoag config set agent.max_rounds 5 -y
已写入 /Users/me/.config/jiaoag/config.yaml（备份 config.yaml.bak-20261125-222554）
```

写入保护：

- **默认 dry-run**，只有 `-y` 才落盘。
- 落盘前备份为 `config.yaml.bak-<时间戳>`。
- 临时文件 + rename 原子替换，权限保持 `0600`。
- 替换后立刻重新加载并校验，失败自动回滚并报错（例如 `agent.max_rounds: -1`）。
- 只替换目标那一段值文本，**空行、注释、缩进原样保留**；这不是「重新序列化整份 YAML」。

路径写法与结构体一致：

```
server.listen
agent.max_rounds
providers[0].model
notify.webhooks[1].url
mcp.servers[0].env.GOPATH
```

未知路径会报错并给最相近的候选（`agent.max_round` → 提示 `agent.max_rounds`）。键不存在时会插入到
父块末尾；但**新增列表条目**（例如再加一个 provider）需要 `config edit`。

敏感项 (`api_key` / `api_keys` / `token` / `secret` / `value`) 必须从 stdin 或文件读：

```
jiaoag config set server.token - -y              # 从 stdin
jiaoag config set providers[0].api_key @~/keys/agnes -y
jiaoag config set providers[0].api_key sk-xxx    # 拒绝：会留在 shell 历史里
```

### edit

```
EDITOR=vim jiaoag config edit
```

打开配置，退出后自动跑一次与 `config check` 相同的离线校验；不自动回滚——文件是用户自己改的。

## 命令判定解释器

`jiaoag policy test` 用本机规则解释一条命令，不启动服务端、不改任何文件：

```
$ jiaoag policy test -- go build ./...
命令：go build ./...
判定：allow（放行）
规则：exec.go
策略：builtin-4（hash 2f1b06c9648a）
原因：whitelist: exec.go
目录：~/.config/jiaoag/policy.d

$ jiaoag policy test -- npm publish
判定：require_user（需要审批）
原因：risky_subcommand: npm publish 会对外发布或操作凭据，请先确认目录与参数

$ jiaoag policy test --shell 'ls | wc -l'
判定：require_user（需要审批）
原因：shell: 需要 shell 解释（管道/重定向/多命令），无法静态判定
```

规则目录不存在时按内置模板判定并说明——`policy test` 不会顺手在配置目录里创建文件。
退出码：`allow` 为 0，`require_user` 为 1，参数错误为 2。

## 退出码汇总

| 命令 | 0 | 1 | 2 |
|---|---|---|---|
| `path` / `show` / `edit` | 成功 | 读取或校验失败 | 参数错误 |
| `check` | 无错误（可有 warning） | 有 error 或 `--online` 探测失败 | 参数错误 |
| `set` | dry-run 或写入成功 | 写入/校验失败 | 参数错误（含敏感项用了位置参数） |
| `policy test` | allow | require_user 或加载失败 | 参数错误 |

## 边界

- 不改 `policy.d/*.yaml`：那是规则，不是配置；用 `policy test` 验证，用编辑器或 `jiaoag policy sync` 更新。
- 不改项目级 `.jiaoag/`、`.agents/skills`、`MEMORY.md`：它们属于工作区，不属于宿主配置。
- 不提供 `config get`：`show -format json` + `jq` 已覆盖脚本场景。
- `show` 的「默认值」表示该字段没有出现在文件里；它与内置默认值相同，但不会告诉你默认值是多少（值本身已经打印出来了）。