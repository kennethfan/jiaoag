# 执行后端可插，但只能由宿主选择

没有容器（ADR-0001），`run_command` 与 server 同权限：一行 `rm -rf` 或 `make` 里藏着的 `curl | sh` 都能直接杀掉工作区。短期内唯一能实质收紧的办法是把命令放进一层隔离执行，但隔离手段平台差异极大（macOS `sandbox-exec`、Linux `bwrap` / `seccomp`、远程 runner、未来的容器），写死任何一种都会把可移植性锁进 v1。

因此引入 `ExecBackend` 作为可插执行后端，权限层输出的是「这条命令是否允许执行」，`ExecBackend` 只负责「在哪执行」。两者正交：

```go
type ExecBackend interface {
	Name() string
	// Prepare 只探测可用性与将要施加的约束，不执行
	Prepare(ctx context.Context, cmd Command) (BackendPlan, error)
	Run(ctx context.Context, plan BackendPlan) (Result, error)
}

type BackendPlan struct {
	Backend  string   // direct | sandbox-exec | bwrap | remote:<name>
	Version  string
	Args     []string // 最终 argv（含包装器参数），供审计与审批展示
	Degraded bool     // 是否从隔离路径降级
}
```

## 三条硬约束

1. **宿主选择后端**。工具与插件无权指定自己要在哪跑——否则插件可以把命令悄悄扔回宿主机，隔离形同虚设。后端由配置 + 平台探测决定，`run_command` 只提交 `Command`。
2. **降级必须显式**。`sandbox` 不可用时不得静默回退到 `direct`：返回 `backend_unavailable`，由用户显式选择继续（`--allow-direct` 或飞配置项）。「以为是隔离的」比「根本没隔离」更危险。
3. **后端身份入审计**。每次执行记录 `backend`、版本与最终 argv；事后必须能回答「这条命令当时在哪跑的」。

## 与 ADR-0001 的关系

ADR-0001 说的是「不用容器、权限策略是唯一安全边界」，本文不推翻它，只补一句边界：**隔离是可选加固，不是运行前提**。缺失任何后端，核心功能仍以 `direct` 正常工作；安全强度则明确降级并告知用户。若哪天要强制隔离，那是新 ADR，不是插件默认行为。

## Considered Options

- **v1 强制容器** — 一致性与隔离最好，但单机可用性、启动成本、macOS 体验都会被拖垮，也与 ADR-0001 冲突。
- **工具自己指定后端**（`run_command{backend: "sandbox"}`）— 灵活，但等于把「在哪执行」交给可能不可信的代码，安全属性失效。
- **静默回退到 `direct`** — 实现最省事，但用户会误以为自己被隔离，属于最坏的失败模式。
- **只做 `direct`，等以后再上容器** — 现在最省事，但失去了用插件低成本加固的路径，`rm -rf` 风险一直裸露。

## Consequences

- v1 只有 `direct` 实现，但接口与审计字段现在就留（`backend`、`Degraded`），成本约 20 行。
- v2 加 `sandbox-exec` / `bwrap` 实现，不改 `perm` / `agent`；新增后端 = 新实现 + 平台探测测试。
- 审批界面必须显示将要使用的后端与最终 argv，否则用户批的是「概念上的命令」而不是「实际执行的命令」。
- 后端探测失败属于可观测事件，写审计与 TUI 提示，不是静默。
- 远程 runner 会把「副作用发生在别的机器」引入模型，届时需要新的审计字段（主机标识），留到该后端落地时再定。
