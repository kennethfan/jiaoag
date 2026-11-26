# 技能包

技能是写给模型的操作手册：什么时候用、按什么步骤做、附带哪些脚本和参考文档。jiaoag 不发明格式，直接读 `SKILL.md`——和 Claude Code / OpenCode 的技能库通用。

## 怎么工作

1. **启动/建任务时扫描**：默认扫 `~/.agents/skills`，每个子目录里的 `SKILL.md` 就是一个技能。
2. **索引进系统提示**：只放「名字 + 一句话适用场景」，追加在内置核心之后。模型知道有哪些技能，但看不到正文。
3. **按需加载正文**：任务与某个技能相符时，模型调用 `skill` 工具取正文。返回内容包含技能的**绝对目录**与一级目录清单，正文里的 `scripts/x.py` 这类相对路径以此为准。
4. **也可以由用户点名**：`/skill` 面板选一个技能，下一条任务会把正文直接拼进系统提示，不依赖模型自觉（见下文「主动调起」）。
5. **不扩大权限**：`skill` 是纯读取，不需要审批。技能里让模型跑脚本时，仍然走 `run_command` 的 argv 白名单与审批。

## 配置

```yaml
agent:
  # auto（默认）注入索引并注册 skill 工具；off 关闭。
  skills: auto
  # 技能目录；留空用 ~/.agents/skills。
  # ~ 在启动时展开；相对路径按会话工作区根解析。
  skills_dirs:
    - ~/.agents/skills
    - .jiaoag/skills
```

`SKILL.md` 的最小格式：

```markdown
---
name: pdf-report
description: 需要处理 PDF 报表时使用。覆盖抽取、合并、加水印。
---

# 步骤

1. 运行 `scripts/extract.py <文件>` 取表格
2. ……
```

- `name` 决定 `skill` 工具的参数取值，必须是字母数字开头、只含 `._-`，最长 64；不合格时退回目录名。
- `description` 会进索引（截断到 120 字），写得越具体越容易被正确选中。
- 缺 frontmatter 也能用：用目录名当名字，没有描述。
- 文件是手写的，`description` 里带 `Triggers: a, b` 这种未加引号的冒号不会导致解析失败。
- `disable-model-invocation: true` 表示「只能用户手动调用」：这类技能不进模型索引（只在末尾留一句「另有 N 个标记为仅手动」），模型不会自选，但用户可以用 `/skill` 指定，`skill` 工具也仍能读。

## 主动调起

模型自动挑技能靠的是 `description` 判断，有时候你想指定某个技能——比如 `grilling`（反复追问方案），或者那些标了 `disable-model-invocation` 的技能。

```text
› /skill                    # 弹面板列出全部技能，[仅手动] 标记不可自选的
› /skill grilling           # 直接指定，不弹面板
› /grilling 这个设计怎么拆   # 命令式：加载 grilling 正文并立刻提交这条任务
› /grilling                 # 不带问题则先指定，等你输入问题后回车
› x                        # 面板里按 x 清除已指定的技能
```

`disable-model-invocation: true` 的技能（本机 15 个）会同时出现在 `/` 补全里，行尾标 `技能`；内置命令排在前面且重名时优先，技能名顶不掉 `/models` 这类真命令。补全列表在会话建立后由服务端 `skill.list` 拉取，项目级技能（`.jiaoag/skills`）也会包含在内。

指定后状态行显示 `· 技能 grilling`，**下一条任务**生效（只影响一条，提交后自动清除）。区别在于：

| | 模型自己调 `skill` | 用户 `/skill` 指定 |
|---|---|---|
| 触发 | 模型判断相符 | 用户决定 |
| 成本 | 多一次工具往返 | 首轮请求就带正文 |
| 挑错风险 | 可能挑错或不挑 | 不会 |
| `Manual` 技能 | 不可用（不进索引） | 可用 |

服务端在任务提交时解析名字：技能库里找不到就整条拒绝（`bad_request`，错误里带名字），任务不会落库。一次最多 4 个。技能正文进入系统提示的 `user:skill:<name>` 层，位于技能索引之后、`MEMORY.md` 之前——能补充，不能覆盖安全边界。

指定的技能记在 `tasks.skills`，服务端重启后续跑会按原样重新注入；也随 `task.queued` 事件可见，TUI 会打一行 `⋯ 加载技能：grilling`。

## 成本

索引常驻，正文按次加载：

| 项 | 量级 |
|---|---|
| 65 个技能的索引 | 约 10 KB（≈3.4k token） |
| 单个技能正文 | 首次加载时一次性进上下文，上限 96 KB |
| 硬上限 | 索引 10 KB，超出会截断并提示缩小目录 |

技能库很大时，先精简 `description`，再考虑拆 `skills_dirs`（不同项目只挂需要的目录）。

## 观测

技能加载就是一次普通的工具调用，事件里有完整记录：

```sql
-- 某次任务加载了哪些技能
SELECT at, json_extract(payload,'$.name') FROM events
WHERE type='tool.requested' AND json_extract(payload,'$.tool')='skill' ORDER BY seq DESC LIMIT 20;
```

`task.queued` 里的 `system_prompt_hash` 与 `extensions_digest` 覆盖技能索引：技能集合变了，指纹就变了，能回答「这一轮能用哪些技能」。

TUI 里加载技能只显示一行 `↳ skill 完成 · 技能正文已载入`，不把正文铺在对话里。

用户指定的技能同时记在任务行上：

```sql
SELECT id, prompt, skills FROM tasks ORDER BY created_at DESC LIMIT 10;
```

## 边界

- 技能只是文本，不带来新工具、新后端、新权限。
- 不做语义检索、不做自动触发（靠模型按 `description` 判断）；没有版本管理、没有技能间依赖。
- `/skill` 列的是**服务端**技能库的内容；客户端不解析技能目录。
- 工作区内的技能目录必须显式写进 `skills_dirs`：仓库自带的指令文本属于 prompt 注入面，克隆下来不该自动生效。
- 技能正文会进 `tool.result` 事件，可回放可审计，也会占事件日志体积。