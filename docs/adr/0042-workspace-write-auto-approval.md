# ADR-0042：工作区与临时目录内的写入默认免审批

状态：已采纳（v2.40）

## 背景

到 v2.39 为止，文件写入的判定是 `perm.Evaluate` 的第一条：`p.Change != nil → RequireUser`，
而且规则引擎只能收紧不能放宽（ADR-0010）。这条硬底线来自 ADR-0001 的信任模型——agent 的
错误改动是静默损失，diff 审批是唯一的「发生前看一眼」的机会。

实际使用里这条底线的位置不对：长任务在自己的工作区里连续改十几个文件，每次都要点一次
「批准一次」，用户会退化成无脑按 `y`，审批从把关变成仪式。真正会静默出事的是少数路径：
`.git/`（hooks、config 等于改执行行为）、`.env*`、密钥、`AGENTS.md` / `.jiaoag/`（改变后续
提示与工具）、CI 工作流（改一次就能在 CI 里跑任意命令）。

## 决策

1. 新增 `agent.write_approval`，三档：
   - `workspace`（默认）：会话全部根（主根 + 附加根）与临时目录内的写入直接落盘；
   - `always`：每次都审批（v2.39 及之前的行为）；
   - `off`：从不审批，连敏感路径也不问（只给完全信任的环境）。
2. **库的零值必须是安全的那一档**：`agent.Options.WriteApproval == ""` 按 `always` 处理，
   由配置层显式传入 `workspace` 作为默认。这样直接构造 `agent.Options` 的调用方（测试、
   嵌入式使用）不会因为忘了设字段而悄悄放宽。
3. 允许免审的根：`workspace.New` 已解析过符号链接的会话根（含附加根），加上
   `os.TempDir()`、`/tmp`、`/private/tmp`、`/var/tmp`、`/private/var/tmp`（同样解析符号链接）。
   路径判定前先解析目标路径的符号链接；文件还不存在时解析父目录——否则「工作区里放一个
   指向 `/etc` 的软链」就能绕过。比较按目录边界，取最深匹配的根，避免 `/wsx` 命中 `/ws`。
4. 敏感路径在 `workspace` 档仍逐次审批：
   - 任一层路径是 `.git`、`.jiaoag`、`.agents`；
   - 以 `.github/workflows/` 开头；
   - 文件名为 `AGENTS.md`、`MEMORY.md`、`.npmrc`、`.netrc`、`.git-credentials`、`.pgpass`；
   - `.env` 与 `.env.*`；
   - 以 `id_rsa` / `id_dsa` / `id_ecdsa` / `id_ed25519` 开头；
   - 以 `.pem` / `.p12` / `.pfx` / `.keystore` / `.jks` / `.kdbx` / `.ppk` 结尾。
5. 免审写入**不是静默写入**：
   - 仍算 diff、仍做 CAS（提出改动后文件被改过就作废）；
   - 仍写 `proposal.created`（带 diff）与 `proposal.applied{auto: true, mode}`；
   - 审计写 `write.auto`（`actor = policy`，detail 含 `path`/`base_hash`/`new_hash`/`bytes`/`mode`），
     与人工批准的 `write.applied` 分开，方便单独查询「哪些改动没人看过」；
   - TUI 与 Web 对话里显示一行「自动应用（工作区内）/（临时目录内）：path」。
6. 任务状态不新增分支：免审写入全程停在 `running`，不产生 `approval` 行，也不进入
   `waiting_approval`。
7. 命令与外部工具不受影响：`run_command` / `check_command` 仍走白名单与审批，
   MCP / 插件的外部调用仍按 `trust` 判定。这个开关只管文件写入。
8. 与 sandbox 正交：`exec.backend: sandbox` 限制「能写到哪些目录」，本开关只决定
   「要不要人看一眼」。两者可以同时开，也可以都不开。
9. 失败路径与人工审批一致：CAS 不匹配或落盘失败时，提案标 failed、工具返回错误，
   审计写 `write.failed`（`actor = policy`）。

## 后果

- 长任务不再被工作区内的写入打断；需要看的写入（敏感路径、工作区外）仍然会停下。
- diff 只能**事后**从事件与审计里查，不再保证「发生前看过」。这是这次明确接受的取舍。
- `extensions_digest` 没有纳入 `write_approval`：它改变的是审批流程，不是提示词、工具集
  或执行后端。需要追溯时看 `write.auto` / `write.applied` 的 `mode` 与 actor。
- `allow_session` 只对仍需审批的写入有意义；工作区内写入不再产生授权记录。
- 关闭审批不等于关闭审计：`jiaoag events` 与 SQL 都能回答「谁在什么时候改了哪个文件」。

## 备选方案

- **路径白名单**（只放行某些目录）：配置会随项目膨胀，且和工作区/附加根重复。
- **只对新建文件免审**：仍要审批「改写既有文件」，而改写恰恰是长任务的主要动作。
- **按 diff 行数阈值**：阈值判断本身容易出错，等于把安全边界交给启发式。
- **默认 `always`、仅提供开关**：用户明确要求默认不打扰，故不采纳；但库层零值仍保持
  `always`，避免实现层意外放宽。