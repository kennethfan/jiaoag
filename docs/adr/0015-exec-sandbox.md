# 执行隔离：sandbox-exec 与 bwrap

ADR-0011 把「在哪执行」抽成 `ExecBackend`，但 v1 只有 `direct`。本文落下第一条真正收紧边界的实现：把命令放进平台自带的沙箱，默认仍关着，由宿主显式开启。

## 背景

`direct` 与 server 同权限：一条 `rm -rf`、一个 `make` 里藏着的 `curl | sh`、一次 `npm install` 的 postinstall，都能改到工作区以外的任何东西。没有容器（ADR-0001），能做的最便宜加固是用操作系统已有的进程级沙箱：macOS 的 seatbelt、Linux 的 bubblewrap。

两者都不是容器，也不提供跨用户隔离；它们只回答「这个进程能不能写这个路径、读那个文件」。对单用户编码 agent 来说，这正好覆盖最现实的损失：误删家目录、密钥被上行到模型。

## 决策

1. **新增 `sandbox` 后端，默认不启用。**
   配置 `exec.backend: sandbox` 才启用；`direct` 仍是默认值。工具与插件无权选择后端（ADR-0011 硬约束 1）。

2. **探测失败即启动失败，绝不回退。**
   `NewSandbox()` 会真实跑一次最小命令验证沙箱可用（macOS 跑 `sandbox-exec /usr/bin/true`，Linux 跑带命名空间的 `bwrap /bin/true`）。失败返回 `ErrNotAvailable`，server 启动报错。用户要跑就直接改配置，不需要「假装隔离」的中间态（ADR-0011 硬约束 2）。

3. **可写范围：工作区 + 临时目录 + 工具链缓存。**
   具体来自命令自己的 `Env`：
   - `Command.Cwd`（命令工具固定为工作区根）
   - `TMPDIR`、`GOCACHE`、`GOMODCACHE`、`GOPATH`、`GOTMPDIR`
   - 平台临时目录（`/tmp`、`/private/tmp`、`/var/tmp` 等）
   - `$HOME/Library/Caches`、`$HOME/go`、`$HOME/.cache`、`$HOME/.npm`
   - `/dev/null`（单独放行，它不在任何可写根下）

4. **禁读：密钥与云凭据目录。**
   `~/.ssh`、`~/.aws`、`~/.gnupg`、`~/.config`、`~/.kube`、`~/.docker`、`~/.netrc`、`~/.npmrc`、`~/.git-credentials`、`~/.password-store`、`~/Library/Keychains`。
   若工作区恰好落在其中某个目录下，放弃该条 deny——不能为了防泄漏把工作区本身封死。

5. **路径必须按真实路径匹配。**
   macOS 上 `/tmp`、`/var` 是 `/private/*` 的符号链接，seatbelt 的 `subpath` 匹配的是解析后的路径。可写根与禁读目录都同时登记原路径与 `EvalSymlinks` 结果，否则规则会静默失效（实测踩过）。

6. **网络不隔离。**
   用户明确选择允许联网：拉依赖、跑测试、访问本地服务都需要。要断网是另一个决策，不藏在隔离实现里。

7. **实现形态。**
   - macOS：`sandbox-exec -p <SBPL>`，SBPL 为 `(allow default)` + `(deny file-write*)` + 可写根白名单 + 敏感目录 `(deny file-read*)`。
   - Linux：`bwrap --die-with-parent --new-session --unshare-pid --unshare-uts --ro-bind / / --dev /dev --proc /proc`，敏感目录用 `--tmpfs` 覆盖，可写根用 `--bind` 挂回，`--chdir` 到工作区。
   - 其他平台：`NewSandbox()` 返回 `ErrNotAvailable`。

8. **资源限制仍叠加在沙箱外层。**
   `runPlan` 先做 `wrapLimits`（`ulimit` 由子进程继承），再交给沙箱包装器；`Plan.Argv` 保持用户命令原样，审批与审计看到的仍是「要跑什么」，`backend` / `backend_version` 说明「在哪跑」。

## 审计与可观测

- `exec.allow` / `exec.approved` 等审计的 `backend` 字段为 `sandbox`，`backend_version` 为 `sandbox/seatbelt-darwin` 或 `bwrap/<version>`。
- 启动日志的 `执行环境已就绪` 行会带 `backend=sandbox`；探测失败时进程不会起来，错误信息说明缺什么。
- 审批面板继续显示后端名与逻辑命令；包装器 argv 由后端与版本确定，不落库。

## 后果

- 命令只能在允许的根里写；`go build`/`go test`、npm 缓存、临时文件都能正常工作（已用真实 provider 的 `make demo` 验证）。
- 读取密钥目录会得到 `Operation not permitted`，命令可能因此报错——这是预期行为，不是后端故障。
- macOS 的 `sandbox-exec` 虽被 Apple 标记为 deprecated，但仍是系统自带、无需安装的唯一进程级沙箱；`bwrap` 需要内核允许非特权用户命名空间，容器/CI 里可能不可用，此时启动报错并由用户决定是否回到 `direct`。
- 这不是容器：不隔离网络、不隔离 PID 之外的内核面、不防同用户提权。它降低误伤与顺手泄漏的概率，不改变「权限策略是唯一安全边界」（ADR-0001）。
- 写白名单是枚举式的：工具链换了缓存位置（例如自定义 `GOCACHE` 未导出到命令 Env）就会写失败。失败是显式的 `Operation not permitted`，按需补路径，不静默放开。

## Considered Options

- **默认开启 sandbox** — 最安全，但会让没装 `bwrap` 的 Linux 用户直接起不来；ADR-0011 明确隔离是可选加固。
- **只做 macOS** — 实现最快，但 Linux 是服务端主场景，缺口太大。
- **用容器（Docker/nsjail）** — 隔离更彻底，但引入运行时依赖与启动成本，与 ADR-0001 冲突。
- **只读整个文件系统、工作区用 overlay** — 写入语义会变（改动不落盘），与「agent 直接改真实文件」的产品前提冲突。
- **探测失败时自动回退 `direct`** — 用户会误以为自己被隔离，是最坏的失败模式（ADR-0011）。