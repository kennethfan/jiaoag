# 多仓库：只读附加根

一个会话默认只有一个工作区根。**附加根（additional root）**让你在不复制目录、不切会话的前提下，
只读访问另外几个仓：`read_file` / `grep` / `glob` 都能用 `别名:相对路径` 指向它们。

写入与命令仍然只限主根——附加根是「看」的通道，不是第二个写入口（ADR-0036）。

## 用法

启动 TUI 时挂载，可重复：

```bash
jiaoag-tui -root lib=../shared-lib -root docs=~/notes/repo
```

脚本与测试用 `jiaoag-demo`：

```bash
jiaoag-demo -workspace . -root lib=../shared-lib -prompt "看看 lib:internal/x.go 怎么实现的"
```

进了 TUI 以后用 `/roots` 看当前挂了什么。

## 工具行为

| 工具 | `alias:path` | 说明 |
|---|---|---|
| `read_file` | ✅ | `read_file {"path":"lib:internal/app.go"}` |
| `grep` | ✅ | `grep {"pattern":"func Login","path":"lib:."}`，结果行带 `lib:` 前缀 |
| `glob` | ✅ | `glob {"pattern":"lib:**/*.go"}`，结果带 `lib:` 前缀 |
| `write_file` / `edit_file` | ❌ | 报 `workspace: 附加根只读` |
| `run_command` / `check_command` | ❌ | cwd 永远是主根，没有「在附加根里跑」的入口 |
| `git_status` / `git_diff` / `git_log` | ❌ | 只看主根仓库 |

不带 `alias:` 前缀时行为与以前完全一致；`Z:foo`、`a b:foo` 这类不是合法别名形态的前缀
按主根下的普通文件名处理（Windows 盘符不会误判）。

前缀看起来像别名但没挂载（例如 `libx:a.go`）会直接报「未知的根别名」，不会退化成主根文件名。

## 约束

- 别名：小写字母开头，字母数字与 `_-`，最多 32 字符；不能与主根或其他附加根重复。
- 路径必须是已存在的目录；创建会话时就校验，不是第一次读取才报错。
- 与主根是同一个目录、或两个别名指向同一个目录，都会被拒绝。
- 最多没有硬上限，但每多一个根就多一份模型可能误用的路径空间，建议只挂真正需要的。

## 不参与的部分

- **命令与 `path_args`**：命令仍只在主根执行，沙箱可写范围也仍只含主根（ADR-0015）。
- **项目级扩展**：`AGENTS.md` / `.jiaoag/` 仍只从主根读一份，附加根里的不会被加载。
- **`extensions_digest`**：挂载哪些附加根不影响任务指纹。

可写附加根、命令在指定根下执行，见 issue #13。

## 观测

附加根的读取不产生审计（只读工具本来就不审）。要看某次任务挂了哪些根：

```sql
SELECT id, workspace, roots FROM sessions ORDER BY created_at DESC LIMIT 5;
```

`roots` 是 JSON 文本，如 `[{"alias":"lib","path":"/Users/me/shared-lib"}]`。

## 边界

- 只读是 agent 工具层的约束，不是文件系统沙箱：`!` 本机命令（客户端执行）不受此限制。
- 附加根里的路径不出现在系统提示里，模型只知道「用户可能提过 `lib:` 这样的前缀」；
  任务提示里最好明确写清别名含义。
- 符号链接在建立会话时解析一次，之后路径变化不会自动生效。