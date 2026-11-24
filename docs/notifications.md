# Webhook 通知

任务在等审批、或者跑完了，而你不在电脑前——这个页面解决的就是这件事。
服务端把事件 POST 到你配置的 HTTP 端点，手机上的 IM 机器人、自建转发器都能接。

只推两类事件，其它一概不发：

| 类别 | 触发 | 何时最有用 |
|---|---|---|
| `approval` | `approval.requested` | 任务卡在等你按 y/n |
| `completed` | `task.status` → `completed` | 长任务跑完了 |
| `failed` | `task.status` → `failed` | 模型/工具/额度出错 |
| `canceled` | `task.status` → `canceled` | 任务被取消或进程重启收尾 |

## 配置

```yaml
notify:
  webhooks:
    - name: ops
      url: https://example.com/jiaoag/hook
      secret: file:~/.config/jiaoag/hook.secret   # 可选
      events: [approval, failed]                   # 可选，留空表示全部
      timeout_s: 10                                # 1–60，默认 10
      enabled: true
```

- `name` 只用于日志，须唯一，字符集 `[A-Za-z0-9_-]`，≤32。
- `url` 只接受 `http` / `https`。
- `secret` 支持明文、`$ENV_NAME`、`file:/path`；配了就对请求体做 HMAC-SHA256。
- `events` 取 `approval` / `completed` / `failed` / `canceled`。

## 载荷

```json
{
  "event": "approval.requested",
  "kind": "approval",
  "at": 1795500000000,
  "session_id": "ses_ab12",
  "task_id": "tsk_cd34",
  "approval_id": "apr_ef56",
  "tool": "go",
  "prompt_excerpt": "修一下 stats.Sum 的边界，测试跑通后提交"
}
```

- `event` 是领域事件名；`kind` 是上面表格里的类别。
- `tool` 取审批对象里最有辨识度的东西：外部工具看工具名，命令看 `argv[0]`，
  写入看路径。
- `prompt_excerpt` 是任务提示词压成一行后的前 120 字。
- 终态通知带 `status`（`completed` / `failed` / `canceled`），不带 `approval_id`。

### 验签

```python
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(expected, headers["X-Jiaoag-Signature"])
```

## 投递语义

- **异步**：通知在事件落库、广播之后入队，投递失败不影响任务。
- **有界**：内存队列 256 条，满了丢新通知并记 warn 日志。
- **重试**：5xx、429、网络错误最多 3 次（间隔 500ms、1s）；4xx 不重试。
- **关闭时尽量投完**：服务端退出时会等待队列里的通知投递完成；
  进程被强杀则未投递的丢失。
- **不落库**：投递结果只在服务端日志里，不进事件日志也不进审计。

## 排障

服务端日志：

```text
WARN 通知投递失败 hook=ops event=approval.requested err="HTTP 502: bad gateway"
WARN 通知队列已满，丢弃 event=task.status task=tsk_cd34
```

先用 `curl` 手动打一次，确认接收端可达与验签逻辑：

```bash
curl -v -X POST https://example.com/jiaoag/hook \
  -H 'Content-Type: application/json' \
  -H 'X-Jiaoag-Event: task.status' \
  -d '{"event":"task.status","kind":"completed","at":0,"task_id":"tsk_x"}'
```

## 边界

- 不做去重：网络重试可能让接收端收到同一条两次；需要幂等请按 `event + task_id + at` 判重。
- 不做分级/静音/免打扰：要静音就 `enabled: false`。
- 不推 diff、argv、工具输出：需要细节回 TUI，或用 `jiaoag events` 导出。
- 提示词摘要会离开本机；敏感环境可以只订阅 `failed`。