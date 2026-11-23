# 跨会话项目记忆

agent 每个任务都从会话事件重建上下文，会话之间不共享历史。`MEMORY.md` 补上「这个项目长期成立的事实」：构建命令、目录约定、踩过的坑、已定决策。

## 行为

- 文件位置：工作区根目录 `MEMORY.md`。
- 每个任务开始时读取，作为一层提示词注入（在内置核心之后，不能覆盖安全边界与审批规则）。
- 文件不存在：不注入，不报错。
- 超过 8 KB：只加载前 8 KB（按字符边界回退），并在正文末尾附一句精简提示。
- 修改记忆只影响之后提交的任务；已在跑的任务沿用提交时冻结的提示词哈希。
- 记忆按工作区隔离：不同仓库的 `MEMORY.md` 互不可见。

## 写入

模型通过 `remember` 工具追加一条记忆：

```json
{"text": "跑测试用 make check；不要直接 go test ./...，会跳过 staticcheck"}
```

流程与写文件一致：

1. `remember` 生成提案，展示 diff（新增一行 `- YYYY-MM-DD: ...`）。
2. 用户在 TUI 里 `y` / `a` / `n` 裁决。
3. 批准后宿主做 hash CAS 与原子写；文件在批准前不会变化。
4. 重复内容、空文本、单条超过 500 字、追加后超过 8 KB，都会被拒绝并提示模型。

模型不能整文件覆盖记忆，只能追加。用户可以直接编辑 `MEMORY.md`，不受审批约束。

## 文件格式

无固定 schema，约定每条一行：

```markdown
- 2026-11-20: 构建用 make check，包含 gofmt / vet / staticcheck / tidy
- 2026-11-21: internal/agent 的测试不要连真实 provider，用 scriptedProvider
- 2026-11-22: 提交前必须等用户确认，不要自动 push
```

日期只用于判断新旧；正文按原样进入提示词。建议不写密钥、口令、个人隐私，必要时把文件加入 `.gitignore`。

## 配置

```yaml
agent:
  memory: auto   # auto（默认）| off
```

- `auto`：注入 `MEMORY.md`，并注册 `remember` 工具。
- `off`：不注入、不注册工具；文件保留，重新开启即恢复。

## 观测

记忆变化会改变系统提示哈希，进而改变任务的 `extensions_digest`：

```sql
SELECT id, prompt, system_prompt_hash, extensions_digest
FROM tasks
ORDER BY created_at DESC
LIMIT 10;
```

写入沿用现有事件与审计：`approval.requested`（diff）→ `approval.resolved` → `proposal.applied`。查某次记忆写入：

```sql
SELECT seq, type, payload
FROM events
WHERE type IN ('proposal.applied', 'approval.resolved')
ORDER BY seq DESC
LIMIT 10;
```

## 边界

- **不做语义检索**：全量注入，相关性由模型判断；装不下时先精简条目。
- **不做自动清理**：过期事实需要人或模型显式删除（目前只能编辑文件或提写文件提案）。
- **不记录任务进度**：进度属于事件日志；记忆只放长期成立的事实。
- **不是跨仓库知识库**：换工作区就是另一份记忆。
- **不是安全边界**：记忆内容可能误导模型；真正的约束仍是权限策略、审批与沙箱。
