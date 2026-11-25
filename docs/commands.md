# 命令：白名单、高风险提示与后台任务

## 白名单

`run_command` / `check_command` 的 `argv` 形态会先过命令白名单（`internal/perm/default.yaml`）。命中即执行，不弹审批：

| 程序 | 放行的子命令 | 备注 |
|---|---|---|
| `go` | `test`、`vet`、`run`、`build`、`mod` | `-o`、`-toolexec`、`-exec` 等仍拒绝 |
| `npm` / `pnpm` | 任意 | 发布、凭据类子命令仍审批 |
| `mvn` / `mvnw` | 任意 goal | `deploy`、`release` 仍审批 |
| `gradle` / `gradlew` | 任意 task | `publish`、`uploadArchives` 仍审批 |
| `make` | 任意目标 | `install`、`uninstall` 仍审批 |
| `ls`、`cat`、`head`、`wc` | — | flag 与路径都在约束内 |

三条硬约束始终生效：

1. `argv[0]` 必须是程序名本身；`./go`、`/tmp/go` 一律审批。
2. 未列入允许列表的 flag 一律审批（`--output`、`-exec` 这类会改变写到哪里）。
3. 非 flag 参数必须落在命令 cwd 之内。

带管道、重定向、通配符的命令仍然只能走 `shell` 字段，且**永远需要审批**——它无法静态判定。

## 高风险子命令

不设黑名单。以下子命令放行，但每次都会落到审批，并在理由里写明风险：

```
risky_subcommand: npm publish 会对外发布或操作凭据，请先确认目录与参数
```

- npm/pnpm：`publish`、`unpublish`、`exec`、`x`、`dlx`、`login`、`adduser`、`token`、`owner`、`access`、`deprecate`、`dist-tag`、`deploy`
- mvn：`deploy`、`release`、`versions:set`（`release:prepare` 这类按前缀命中）
- gradle：`publish`、`uploadArchives`、`deploy`
- make：`install`、`uninstall`

判据只有一条：**会让东西离开这台机器、或写到工作区外**的动作保留人工确认；其余归「误操作」范畴，靠审计与 `git` 兜底。这也是为什么 `npm install` 不拦——它会跑依赖的 `postinstall`，但在当前威胁模型下这也只算「跑一次仓库代码」。

`make` 免审有一个配套约束：`Makefile`、`makefile`、`GNUmakefile`、`*.mk` 属于敏感写入，改它们仍要看 diff。否则「写 Makefile → 下一轮 `make` 自动执行」就是一条免审的任意代码执行链路。

## 后台任务

`npm run dev`、`mvn spring-boot:run` 这类长驻命令用后台模式：

```
run_command {"argv":["npm","run","dev"],"background":true}
```

返回：

```
已启动后台命令（id call_9f3c…）：npm run dev
输出持续写入日志：用 read_output 传 tool_call_id=call_9f3c… 查看；
需要停止时调用 stop_command（同样传这个 id）。
```

- `background` 与 `timeout_s` 互斥。
- id 就是 `tool_call_id`，事件、审计、日志都用它定位。
- 生命周期绑会话：任务结束继续跑；会话删除、`jiaoag serve` 关闭、空闲退出时整体终止；重启不接管。
- 单会话最多 4 个，超出报错（不会排队）。
- 后台进程**不算忙**，不会阻止空闲退出；但服务端退出时会先杀掉它们。
- 输出只进日志，不进对话流；日志默认上限 4 MB，超出后丢弃并写提示。

## 观测

```sql
-- 最近的后台启动
SELECT ts, json_extract(detail,'$.argv'), json_extract(detail,'$.background')
FROM audit WHERE action IN ('exec.allow','exec.approved')
  AND json_extract(detail,'$.background') = 1
ORDER BY id DESC LIMIT 20;

-- 某次后台启动的日志路径
SELECT output_path FROM tool_calls WHERE id = 'call_9f3c…';
```

## 边界

- 白名单是静态数据，不随项目配置变化；要调整只能改规则文件或等新版本。
- 后台进程没有自动重启、健康检查、端口管理；它只负责「启动起来」。
- Windows 关闭 Job 会连带收掉残留子进程；Unix 在父进程自行退出时可能留下子进程。
- 免审命令仍然会写工作区产物（如 `go build` 的二进制），这些不走 diff 审批，只留 `exec.*` 审计。