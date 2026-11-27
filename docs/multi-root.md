# 多仓库：附加根

一个会话默认只有一个工作区根。**附加根（additional root）**让你在不复制目录、不切会话的前提下
同时读写几个仓：所有工具都用 `别名:相对路径` 指向它们。

- v2.24（ADR-0036）先做了只读访问；
- v2.25（ADR-0037）把附加根开放为可写，命令也能用 `root` 指定在哪个根下执行。

## 用法

启动 TUI 时挂载，可重复：

```bash
jiaoag tui -root lib=../shared-lib -root docs=~/notes/repo
```

脚本与测试用 `jiaoag demo`：

```bash
jiaoag demo -workspace . -root lib=../shared-lib -prompt "看看 lib:internal/x.go 怎么实现的"
```

进了 TUI 以后用 `/roots` 看当前挂了什么。

## 工具行为

| 工具 | `alias:path` | 说明 |
|---|---|---|
| `read_file` | ✅ | `read_file {"path":"lib:internal/app.go"}` |
| `grep` | ✅ | `grep {"pattern":"func Login","path":"lib:."}`，结果行带 `lib:` 前缀 |
| `glob` | ✅ | `glob {"pattern":"lib:**/*.go"}`，结果带 `lib:` 前缀 |
| `write_file` / `edit_file` | ✅ | diff 头与审批面板显示 `lib:...`；落盘、CAS 都在那个根里 |
| `run_command` / `check_command` | ✅ | 加 `"root":"lib"` 在附加根里执行，cwd 与 `path_args` 判定边界都切过去 |
| `git_status` / `git_diff` / `git_log` | ❌ | 只看主根仓库（多根 git 还没做） |

不带 `alias:` 前缀时行为与以前完全一致；`Z:foo`、`a b:foo` 这类不是合法别名形态的前缀
按主根下的普通文件名处理（Windows 盘符不会误判）。

前缀看起来像别名但没挂载（例如 `libx:a.go`）会直接报「未知的根别名」，不会退化成主根文件名。

## 约束

- 别名：小写字母开头，字母数字与 `_-`，最多 32 字符；不能与主根或其他附加根重复。
- 路径必须是已存在的目录；创建会话时就校验，不是第一次读取才报错。
- 与主根是同一个目录、或两个别名指向同一个目录，都会被拒绝。
- 最多没有硬上限，但每多一个根就多一份模型可能误用的路径空间，建议只挂真正需要的。

## 写入与命令

写入走和主根完全相同的链路：diff → 审批（可逐块）→ CAS → 落盘。区别只在路径：
提案、事件、审计里都是 `lib:internal/x.go`，落盘时按别名找回对应的工作区。

命令用 `root` 选根：

```json
{"argv": ["go", "test", "./..."], "root": "lib"}
```

- `cwd` 变成附加根，`path_args` 白名单规则按**这个 cwd** 判定——在 lib 里跑 `go test ./...` 不会因为参数在主根外被拒。
- 沙箱（`exec.backend: sandbox`）的可写范围扩展为「会话所有根 + 缓存目录」，所以 lib 里的构建产物能正常写。
- 不带 `root` 时行为与以前完全一致。

## 不参与的部分

- **项目级扩展**：附加根的 `AGENTS.md` / `.jiaoag/` / `MEMORY.md` **也参与**（ADR-0055），
  与主根一起按一次内容摘要确认；同名技能与 MCP server 由主根胜出。
- **git 工具**：`git_status` / `git_diff` / `git_log` 仍只看主根仓库。
- **`extensions_digest`**：挂载的附加根如果带项目级扩展，会通过提示层与 MCP 工具集
  进入任务指纹；只有读写权限的根（没有任何 `.jiaoag/` 与 `AGENTS.md`）不影响指纹。
- **TUI 的「当前根」**：没有这个概念，一律显式写 `alias:` / `root`。

## 观测

读取不产生审计（只读工具本来就不审），写入会写 `write.applied`，命令会写 `exec.*`。
两者的 detail 都带 `root`（主根为空，附加根是别名）：

```sql
SELECT action, json_extract(detail,'$.path'), json_extract(detail,'$.root')
FROM audit WHERE action IN ('write.applied','exec.approved') ORDER BY id DESC LIMIT 10;
```

要看某次任务挂了哪些根：

```sql
SELECT id, workspace, roots FROM sessions ORDER BY created_at DESC LIMIT 5;
```

`roots` 是 JSON 文本，如 `[{"alias":"lib","path":"/Users/me/shared-lib"}]`。

## 边界

- 附加根的可写边界由工具层与（启用时）沙箱共同保证；`!` 本机命令在客户端执行，不受此限制。
- 一个任务里跨根写同一个文件不会互相阻塞：CAS 只在单个根内比较，两个会话同时改同一个文件仍按「后写者 stale」处理。
- 附加根里的路径不出现在系统提示里，模型只知道「用户可能提过 `lib:` 这样的前缀」；
  任务提示里最好明确写清别名含义。
- 符号链接在建立会话时解析一次，之后路径变化不会自动生效。