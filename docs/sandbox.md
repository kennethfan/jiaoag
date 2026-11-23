# 执行隔离（sandbox 后端）

默认的 `direct` 后端让命令与 server 同权限。`sandbox` 后端把命令放进操作系统自带的进程级沙箱：能写的地方只有工作区、临时目录和工具链缓存，密钥目录读不到。它不是容器，网络不隔离。

设计动机与取舍见 [ADR-0015](adr/0015-exec-sandbox.md)。

## 开启

```yaml
exec:
  backend: sandbox   # 默认 direct
```

- macOS：使用系统自带的 `sandbox-exec`（seatbelt），无需安装。
- Linux：需要 `bwrap`（bubblewrap），且内核允许非特权用户命名空间。Debian/Ubuntu：`apt install bubblewrap`。
- 其他平台：启动时明确报 `exec.backend=sandbox 不可用`。

后端不可用时 server **不会启动**，也不会悄悄退回 `direct`。要回到宿主直连，就显式把配置改回 `backend: direct`。

## 允许与禁止

| 操作 | 结果 |
|---|---|
| 写工作区（`Command.Cwd`） | ✅ |
| 写 `TMPDIR`、`/tmp`、`/var/tmp` | ✅ |
| 写 `GOCACHE` / `GOMODCACHE` / `GOPATH` / `$HOME/go` / `$HOME/Library/Caches` / `$HOME/.cache` / `$HOME/.npm` | ✅ |
| 写 `/dev/null` | ✅ |
| 写家目录其他位置、系统目录 | ❌ `Operation not permitted` |
| 读 `~/.ssh`、`~/.aws`、`~/.gnupg`、`~/.config`、`~/.kube`、`~/.docker`、`~/.netrc`、`~/.npmrc`、`~/.git-credentials`、`~/.password-store`、`~/Library/Keychains` | ❌ |
| 网络访问 | ✅（不隔离） |
| 读工作区、系统库、`PATH` 上的二进制 | ✅ |

路径白名单以命令自己的 `Env` 为准（`HOME`、`TMPDIR` 等），并同时登记符号链接解析后的真实路径。工作区若正好在某个敏感目录下，该条禁读会被跳过，避免把工作区封死。

## 验证是否生效

```bash
# 在 agent 里让它执行：
echo hi > inside.txt && cat inside.txt          # 成功
echo bad > ~/outside.txt                        # Operation not permitted
cat ~/.ssh/id_rsa                               # Operation not permitted
```

审计里能查到后端身份：

```sql
SELECT occurred_at, action, actor, detail
FROM audit
WHERE action LIKE 'exec.%'
ORDER BY occurred_at DESC;
```

`detail.backend` 为 `sandbox`，`detail.backend_version` 为 `sandbox/seatbelt-darwin` 或 `bwrap/<version>`。

用一次性 demo 验证：

```bash
JIAOAG_DEMO_EXEC_BACKEND=sandbox \
JIAOAG_DEMO_BASE_URL=https://apihub.agnes-ai.com/v1 \
JIAOAG_DEMO_MODEL=agnes-3.0-flash \
JIAOAG_DEMO_API_KEY_FILE=$HOME/.config/agnes/key \
make demo
```

## 常见问题

**`go build` / `go test` 报 `Operation not permitted`**
工具链把缓存写到了白名单外（常见于自定义 `GOCACHE`、`GOMODCACHE`，或 `GOTMPDIR`）。命令工具的 Env 会透传这些变量；设置它们后重新执行即可。也可以把路径加进 `sandboxPolicyPaths` 的白名单（`internal/exec/sandbox.go`）。

**Linux 上 `bwrap` 报 `Creating new namespace failed`**
内核禁止非特权用户命名空间（部分容器、CI、`kernel.unprivileged_userns_clone=0`）。这是环境限制，不是配置错误；只能改用 `direct` 或提供可用的命名空间。

**沙箱能防住什么？**
误删/误改工作区外的文件、把 `~/.ssh` 之类的内容读进模型上下文、构建脚本顺手写到系统目录。它不防同用户提权，也不防工作区内的破坏性命令——后者仍由权限策略与审批负责（ADR-0001、ADR-0004）。