# 放弃危险命令黑名单，改为默认拒绝 + argv 白名单

在没有容器的情况下，`run_command` 的自动放行判定只能作用在 shell 字符串上。实测语义表明「匹配危险模式再拒绝」这条路不可靠：命令替换（`$(...)`）、反引号、管道接 `sh`、base64 解码执行、`python -c` 都能让一段看起来无害的字符串产生任意副作用，而模型恰好擅长生成这类变体。

因此策略反过来：**默认一律需审批**，只有同时满足两个条件才自动放行——argv 中不含任何 shell 元字符（`| & ; < > $ \` ( ) * ? [ ] ~`），且首个 token 命中白名单（`go test|vet|run`、`ls`、`cat`、`head`、`wc`）。命中不了就落到需审批，而不是落到拒绝——拒绝会让 agent 频繁卡死，审批则把决定权交回用户。

> **落地说明（M5）**：白名单条目落在 `internal/perm/default.yaml`，除程序/子命令外还约束参数——`allow_flags`（未列出的 flag 一律审批）、`deny_flags`（`--output`、`-exec` 等）与 `path_args`（非 flag 参数必须落在工作区根内）。`git branch` 已移出白名单：它带 `-d/-D/-m` 时会改 refs，不是只读命令。白名单仍是安全边界的一部分，条目变更必须同步本 ADR。
>
> **M5 复审后的三处收紧**：
>
> 1. `go build` 移出白名单。它会把产物写到当前目录（`go build ./...` 也会为每个 main 包落一个二进制），等于一条免审批的「写入磁盘」，而且重名时会静默覆盖用户刚批准过的文件。`go test` / `go vet` 只写 GOCACHE，保留放行。
> 2. `git status|diff|log|show` 整体移出 `run_command` 白名单。git 读命令会执行仓库配置里的外部程序：`diff.external` 与 gitattributes 的 diff driver 在 `git diff` 时运行，`core.fsmonitor` 在 `git status` 刷新索引时运行。只读 git 统一走 `internal/tools` 的三个专用工具，它们固定注入 `GIT_CONFIG_K=V core.fsmonitor=false` 与 `--no-ext-diff --no-textconv`，argv 由我们拼、模型只能填受约束的 path/ref/行数。
> 3. 白名单只认**程序名本身**：`argv[0]` 含路径分隔符（`./go test`、`/tmp/x/go test`）一律审批。basename 相同不等于同一个程序，工作区里放一个名为 `go` 的可执行文件不该被当成 go 放行。

## Considered Options

- **危险命令黑名单**（`rm -rf`、`sudo`、`curl|sh`…）— 字符串层面可无限绕过，且会让「不在黑名单里」被误读为「安全」。
- **全部需审批、无白名单** — 最安全，但 `go test` 每跑一次都要点确认，日常使用会被劝退，最终导致用户无条件 `allow_session`，安全性反而归零。
- **容器内执行** — 已被 ADR-0001 排除。

## Consequences

- 白名单是安全边界的一部分，条目增删必须同步本 ADR；M5 上线后需要用真实使用数据校准一次。
- 白名单只对**无元字符的 argv** 生效，这条规则写在判定顺序里，不能作为「先删元字符再匹配」的清洗步骤——清洗会把 `a; rm -rf /` 变成一条合法白名单命令。
- 用户用 `allow_session` 放宽审批时，放宽的是**同一 tool + 同一 args hash**，不是「该工具从此免审」。
