# ADR-0046：配置子命令（config）与命令判定解释器

状态：已采纳（v2.43）

## 背景

配置只有创建入口 `jiaoag serve -init`，没有查看与修改入口。三个真实问题：

1. **它在哪**：默认路径、`~` 展开、`-config` 覆盖，靠文档记。
2. **现在生效的是什么**：`applyDefaults` 的默认值、YAML 字面量、`$ENV` / `file:` 的运行时解析叠在一起，读文件答不出「token 到底用了哪一把」。
3. **这样写对吗**：`ValidateServer` 只在启动时跑，写错字段要先启一次服务端才知道；把 YAML 改坏时错误信息还落在启动日志里。

另外，v2.42 把命令白名单改成用户可编辑的 `policy.d/default.yaml`，却没有一个「我这样写会得到什么判定」的入口，只能跑任务撞。

## 决策

1. 新增 `jiaoag config`，子命令 `path` / `show` / `check` / `set` / `edit`；`jiaoag policy` 增加 `test`。
2. `show` 输出生效值**与来源**：`config.yaml:<行号>`、`默认值`，密钥类只报来源与解析长度（`file:…（32 字节）`），永不打印明文。计算值（`context_budget_tokens` 由 chars 折算）单独列一行，避免「字段是 0」被读成「没设预算」。
3. `set` 只改**已知标量路径**：路径索引由 `Config` 结构体的 yaml tag 反射生成；未知路径报错并给最相近的候选。键存在时用**文本片段替换**（`node.Line`/`Column` 定位），不重新序列化整份 YAML，因此空行、注释位置、缩进都保持不变；键不存在时在父映射末尾插入一行。
4. 写入链固定为：打印 diff（默认 dry-run）→ `-y` 才写 → 备份 `config.yaml.bak-<时间戳>` → 临时文件 + rename → 重新 `Load` + 校验，**校验失败自动回滚**。原文件本来就不可用时允许修改（否则用户没有修配置的工具）。
5. 敏感项（`Secret` 类型，以及 `api_key`/`api_keys`/`token`/`secret`/`value` 命名的字段）强制从 stdin（`-`）或 `@file` 读入，拒绝位置参数——位置参数会留在 shell 历史与 `ps` 里。
6. `check` 默认离线：语法、按角色调用 `ValidateServer`/`ValidateClient`、逐个令牌解析、storage 目录可写、明文监听 + 无 TLS 警告、`write_approval: off` 警告。`--online` 才探测供应商 `/models`、MCP 启动（工具数）、对 `server.listen` 做一次 TLS 握手。
7. `policy test` 只读规则、不创建文件：规则目录不存在时按内置模板判定并在输出里说明。`--` 之后是 argv；`--shell` 按 `sh -c` 形态判定，直接给出「shell 无法静态判定」的真实原因。退出码 `allow=0`、`require_user=1`，便于脚本判断。

## 后果

- 结构性编辑（新增 provider、webhook 条目）仍需 `config edit`：用命令拼 YAML 列表会迅速变成一套自有 DSL，而 `$EDITOR` 已经够用。
- `set` 不支持改多行块（literal/folded 标量）与嵌套结构的替换，遇到时报错并指向 `config edit`，而不是悄悄重排文件。
- 只支持修改宿主配置，不碰 `policy.d/*.yaml`、项目级 `.jiaoag/`、`MEMORY.md`。
- `config show` 的行号来自文件本身；`set` 插入的键没有注释，与 `example.yaml` 的教学注释不是一回事。

## 备选方案

1. **反序列化 `Config` 后整体重写**：实现最简单，但 yaml.v3 会丢掉空行，一份手写注释配置被洗成另一种排版。
2. **纯文本 `sed` 式替换**：要自己处理引号、行内注释、多行块，比用 `yaml.Node` 定位更容易写坏文件。
3. **Web 配置页**：与 v2.39 的 Web 前端复用成本不低，且改配置本身是低频动作，终端命令足够。
4. **`config get <path>`**：`show` 已覆盖；需要脚本取值时用 `show -format json` 加 `jq`。
