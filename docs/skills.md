# 技能包

技能是写给模型的操作手册：什么时候用、按什么步骤做、附带哪些脚本和参考文档。jiaoag 不发明格式，直接读 `SKILL.md`——和 Claude Code / OpenCode 的技能库通用。

## 怎么工作

1. **启动/建任务时扫描**：默认扫 `~/.agents/skills`，每个子目录里的 `SKILL.md` 就是一个技能。
2. **索引进系统提示**：只放「名字 + 一句话适用场景」，追加在内置核心之后。模型知道有哪些技能，但看不到正文。
3. **按需加载正文**：任务与某个技能相符时，模型调用 `skill` 工具取正文。返回内容包含技能的**绝对目录**与一级目录清单，正文里的 `scripts/x.py` 这类相对路径以此为准。
4. **不扩大权限**：`skill` 是纯读取，不需要审批。技能里让模型跑脚本时，仍然走 `run_command` 的 argv 白名单与审批。

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
- `disable-model-invocation: true` 表示「只能用户手动调用」：这类技能不进模型索引（只在末尾留一句「另有 N 个标记为仅手动」），模型不会自选，`skill` 工具仍能读它（用户可以在 prompt 里点名要用它）。

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


## 边界

- 技能只是文本，不带来新工具、新后端、新权限。
- 不做语义检索、不做自动触发（靠模型按 `description` 判断）；没有版本管理、没有技能间依赖。
- 工作区内的技能目录必须显式写进 `skills_dirs`：仓库自带的指令文本属于 prompt 注入面，克隆下来不该自动生效。
- 技能正文会进 `tool.result` 事件，可回放可审计，也会占事件日志体积。