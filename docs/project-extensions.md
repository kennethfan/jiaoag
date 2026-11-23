# 项目级扩展

工作区里可以带上提示、技能与 MCP server，让 agent 进目录就按这个仓库的规矩工作。这是**仓库内容**：默认不信任，确认一次后才生效。

## 放什么、放哪

| 路径 | 作用 |
|---|---|
| `AGENTS.md` | 项目提示。claude-code / codex 等同名文件，直接复用 |
| `.jiaoag/prompts/*.md` | 分段提示，按文件名排序拼接（`10-`、`20-` 即顺序） |
| `.jiaoag/skills/<name>/SKILL.md` | 项目技能，格式与 `~/.agents/skills` 完全一致 |
| `.agents/skills/<name>/SKILL.md` | 跨 agent 共享技能目录，也一并扫描 |
| `.jiaoag/mcp.yaml` | 项目级 MCP server 清单 |
| `MEMORY.md` | agent 自己写的跨会话记忆，不参与项目确认（另见 `docs/memory.md`） |

项目技能**覆盖**机器级同名技能：仓库离得近、写得更具体，冲突时应该由仓库说了算。

## 一个最小例子

```markdown
<!-- AGENTS.md -->
改代码前先跑 `make check`；新增包要在 `docs/design/agent-architecture.md` §3 补一行。
```

```markdown
<!-- .jiaoag/skills/release/SKILL.md -->
---
name: release
description: 按本仓库的流程发版
---

1. 更新 CHANGELOG
2. `make check`
3. 打 tag 并推送
```

```yaml
# .jiaoag/mcp.yaml
servers:
  - name: repo-db
    command: ./scripts/mcp-db
    args: ["--readonly"]
    timeout_s: 60
    trust: ask
```

## 确认流程

```text
TUI 启动 / 进入会话
  ├─ 没发现资产        → 什么都不做
  ├─ project.mode=off  → 什么都不做（连发现都不做）
  ├─ project.mode=auto → 直接加载
  └─ project.mode=ask  → 摘要与 project_trust 记录比对
        ├─ 一致   → 加载
        └─ 不一致 → 跳过，提示「输入 /trust 查看并启用」
```

- `/trust` 确认当前内容摘要；`/untrust` 撤销。
- 摘要覆盖提示的路径与内容、技能的 `SKILL.md` 内容、MCP 的 argv/env/dir。**任何改动都要重新确认**——包括给提示片段改名（会改变拼接顺序）。
- 确认记录存在数据目录的 `project_trust` 表，按工作区路径分开，不写进仓库。

```bash
sqlite3 ~/.local/share/jiaoag/jiaoag.db "SELECT workspace, substr(digest,1,12), trusted_at FROM project_trust"
```

## 配置

```yaml
project:
  mode: ask     # ask（默认）/ auto / off
```

| 模式 | 行为 | 适用 |
|---|---|---|
| `ask` | 首次发现提示确认；内容变化重新确认 | 默认。仓库可能是别人写的 |
| `auto` | 不确认，直接加载 | 自己的仓库，嫌确认麻烦 |
| `off` | 完全忽略项目资产 | 只要全局配置 |

## 生效范围

- 提示进系统提示，位置在技能索引之后、点名技能与 `MEMORY.md` 之前，来源记为 `project:<相对路径>`；进提示词 hash，因此进 `extensions_digest`。
- 技能进索引（`disable-model-invocation` 的只出现在 `/skill` 面板）；正文仍按需加载。
- MCP server 与全局 `mcp.servers` 合并，工具统一命名 `mcp__<server>__<tool>`；清单变化时该工作区的 server 自动重建。
- 未确认的会话里，TUI 会有一行「⚠ 检测到项目级扩展……未确认已跳过」。

## 边界

- 上限：单个提示文件 16 KB、全部提示 32 KB，超出截断并标注。
- 项目级 MCP 默认 `trust: ask`：每次调用仍需批准，除非在 `mcp.yaml` 里写 `trust: allow`——那句话本身也是被确认的内容。
- 不提供项目级供应商密钥、模型选择、工具描述改写：这些属于安全边界或代码。
- 确认是「一次性快照」，不是持续监视：确认后文件被改，下次进入会话会发现摘要不一致并要求重新确认。
- `project.mode: auto` 下不做任何确认，clone 陌生仓库时不要用。