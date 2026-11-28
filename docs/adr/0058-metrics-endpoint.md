# ADR-0058：`/metrics` 指标出口

## 背景

`jiaoag` 有事件与审计导出（`jiaoag events`），但那是「一行一条记录」的流水，回答不了运维最常问的几个问题：进程还活着吗、连接与订阅有多少、库长到多大了、有多少任务卡在 `waiting_approval`、有没有孤儿后台进程。`/healthz` 只回答「活着」，而且匿名就能访问。

## 决策

1. **同端口 `GET /metrics`，Prometheus 文本格式**。不引 `prometheus/client_golang`：指标只有十几个，`# HELP` + `# TYPE` + `name value` 四行语法自己写更可控，也少一个依赖。
2. **必须带令牌**（复用 `authorized(r)`，即 WS 那套 Bearer）。指标不暴露任务内容，但会话数、事件行数、连接数属于侦察材料，与 `/healthz` 的「匿名只回 ok、细节要令牌」分级一致（ADR-0029 的信息分级思路）。
3. **只读令牌也能抓**。权限分级针对的是「改状态」，抓指标是纯读；而且监控进程用只读令牌比用可写令牌更安全。
4. **两类指标**：
   - 进程：`uptime`、`goroutines`、`connections`、`subscriptions`、`memory_alloc/sys`、`gc_cycles`、`info{version,protocol}`；
   - 库：`sessions`、`tasks{status}`、`events`、`audit_rows`、`background_procs{status=running|orphaned}`。
5. **每次抓取实时查库，不做进程内缓存**。`COUNT(*)` 在单用户规模是毫秒级；缓存会引入「指标与库不一致」这类最难查的问题。
6. **库读失败返回 200 + `jiaoag_metrics_error 1`**，而不是 500：进程级指标仍然有效，scrape 的 `up` 也没必要因为一次查询失败翻红；错误状态本身有指标表达。
7. **聚合查询 2 秒超时**。指标抓取不该因为库忙而挂住连接；超时同样落到 `jiaoag_metrics_error`。
8. **不暴露任何内容型数据**：没有提示词、路径、命令、diff。要那些东西用 `jiaoag events`（有审计与保留策略的讨论）。

## 后果

- 可以接 Prometheus / Vector / Datadog agent，做「任务卡在 waiting_approval 超过 N 分钟」「孤儿进程数 > 0」「事件表增长速度」这类告警。
- 每次抓取是一次全表 `COUNT`；在百万行事件表上会变慢，届时要么加缓存、要么改成增量计数器——现在不做，因为没有实测数据支撑。
- 令牌要交给监控系统（`credentials_file`），与 WS 客户端用的是同一套令牌，没有单独的「metrics token」概念。
- 只读指标没有「按会话/按任务」的维度：单用户场景下加标签会让序列数爆炸，需要时再评估。

## 备选

- **OpenTelemetry SDK**：能力更全（指标、追踪、导出器），但会给一个单二进制工具引入可观的依赖与初始化成本，当前需求只有「十几个数字」。
- **把指标塞进 `/healthz`**：省一个路由，但健康检查会被监控系统高频调用，全表 COUNT 不适合放在探针路径上。
- **单独的 metrics 端口**：便于网络层隔离，但多一个监听、多一份配置，与「同端口、同令牌」的简洁性冲突。
- **事件表驱动的计数器（进程内累加）**：省掉 COUNT，但多运行时下进程内计数不完整，得回库校验，反而更绕。