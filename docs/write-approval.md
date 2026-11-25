# 写入审批

文件写入默认在工作区与临时目录内直接落盘，其余位置仍然会停下来让你看 diff
（ADR-0042）。这条规则只管**文件写入**，命令与外部工具（MCP / 插件）的审批不受影响。

## 三档配置

```yaml
agent:
  write_approval: workspace
```

| 值 | 行为 |
|---|---|
| `workspace`（默认） | 会话主根、附加根、临时目录（`TMPDIR`、`/tmp`、`/var/tmp`）内直接写；敏感路径与工作区之外仍逐次审批 |
| `always` | 每次写入都弹 diff（v2.39 及之前的行为） |
| `off` | 从不审批，连敏感路径也不问 |

改完重启服务端生效（`jiaoag serve` / `jiaoag` 自动拉起的那个进程）。

## 判定顺序

1. 模式是 `always` / 未配置 → 审批。
2. 目标路径解析符号链接（文件不存在时解析父目录），落在任一允许根内？
   工作区根由 `workspace.New` 解析过软链；`/tmp` 在 macOS 上是 `/private/tmp` 的软链，也会归一化。
3. 相对该根的路径命中敏感清单 → 审批。
4. 其余 → 直接落盘。

工作区里放一个指向外部的软链不算「工作区内」：工具层的 `workspace.Resolve` 会先拒绝，
即使绕过也会在判定第 2 步落到根外。

## 敏感路径（`workspace` 档仍审批）

- 任一层目录叫 `.git`、`.jiaoag`、`.agents`；
- `.github/workflows/` 下的文件；
- `Makefile`、`makefile`、`GNUmakefile`、`*.mk`（`make` 已免审，改构建入口必须看 diff）；
- `AGENTS.md`、`MEMORY.md`、`.npmrc`、`.netrc`、`.git-credentials`、`.pgpass`；
- `.env` 与 `.env.*`；
- `id_rsa*`、`id_dsa*`、`id_ecdsa*`、`id_ed25519*`；
- `.pem`、`.p12`、`.pfx`、`.keystore`、`.jks`、`.kdbx`、`.ppk`。

理由：这些文件改了会改变「之后会发生什么」（git hooks、项目提示、CI、凭据），
而不是仅仅改坏一段代码。

## 免审 ≠ 无痕

每次免审写入仍然：

- 计算完整 diff 并做 CAS（提出改动后文件被改过就作废，要求模型重新读）；
- 写 `proposal.created`（带 diff）与 `proposal.applied`（`auto: true`、`mode`）；
- 写一条 `write.auto` 审计，`actor = policy`，detail 含
  `path` / `base_hash` / `new_hash` / `bytes` / `mode` / `auto`；
- 在 TUI / Web 对话里显示一行「自动应用（工作区内）/（临时目录内）：path」。

人工批准的写入仍写 `write.applied`，两者分开，方便查「哪些改动没人看过」。

## 回看改了什么

```bash
# 任务的全部写入（人工 + 免审）
sqlite3 ~/.local/share/jiaoag/jiaoag.db "
SELECT datetime(ts,'unixepoch','localtime') AS at, actor, action,
       json_extract(detail,'\$.path') AS path,
       json_extract(detail,'\$.mode') AS mode
FROM audit
WHERE action IN ('write.applied','write.auto','write.failed')
ORDER BY id DESC LIMIT 20;"

# 某个任务的全部 diff
sqlite3 ~/.local/share/jiaoag/jiaoag.db "
SELECT id, path, diff FROM proposals WHERE task_id='tsk_...';"

# 只导出免审写入
jiaoag events -s ses_... -from 0 --until-now | jq 'select(.kind=="audit" and .action=="write.auto")'
```

`proposal.created` 事件里也带完整 diff，重放时可以直接看到改动内容。

## 与 sandbox、命令的关系

| 机制 | 管什么 |
|---|---|
| `agent.write_approval` | 这次写入要不要人看一眼 |
| `exec.backend: sandbox` | 进程**能写到哪些目录**（工作区、临时目录、工具链缓存） |
| 命令白名单 + 审批 | `run_command` 能不能跑、要不要人批 |

三者互相独立：可以把写入全放行但仍用 sandbox 限制越界，也可以关闭 sandbox 但保留敏感路径审批。

## 已知边界

- 审批豁免按路径静态判断，不判断「这次改动是否有害」。模型把一段逻辑改错，工作区内仍然直接落盘。
- 事件与审计是事后追溯，不是事前拦截；配合 git 使用最稳（`git diff` 随时能看）。
- `off` 档连 `.git/hooks` 与 `.env` 都不问，只建议在一次性容器或可丢弃的工作区里用。
- `extensions_digest` 不包含这个配置；追溯时看写入事件里的 `mode` 与审计 actor。