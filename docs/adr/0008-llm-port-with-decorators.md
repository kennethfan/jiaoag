# 供应商走薄适配器，重试与限流放装饰器

需要同时支持 OpenAI 兼容协议与 Anthropic Messages API，未来还可能加第三家。把重试、退避、超时、限流写进每个适配器，会让「协议翻译」与「网络韧性」两件事纠缠：两家各写一遍重试逻辑，行为不可能一致，测试也要为每种组合再造一套假服务端。

因此端口只定义能力，适配器只做翻译，韧性用组合解决：

```go
provider → retryProvider → timeoutProvider
```

`Provider.Chat` 是非流式基线，`Streamer.ChatStream(ctx, req, fn)` 是可选增强；流式统一成 `Chunk{TextDelta, ToolCallDelta, Usage, Done}`，两家的事件粒度差异在适配器内部吸收。

## Considered Options

- **重试写在适配器里** — 每家一套，退避参数漂移，错误分类（限流 / 鉴权 / 上下文超限）容易各写各的。
- **引入第三方 LLM SDK / 网关** — 少写代码，但流式工具调用与 usage 的细节被隐藏，出错时只能黑盒调试；且 ADR-0002 已定从零重写。
- **只支持 OpenAI 兼容协议** — 最简单，但把 Anthropic 用户排除在外，而它是当前主要可选项之一。

## Consequences

- 适配器零重试、零限流：只负责请求/响应与流的字段映射，出错原样返回类型化错误。
- 错误分类在装饰器里统一：`ErrRateLimited`（带 `RetryAfter`）可重试，`ErrAuth` / `ErrBadRequest` 不重试，`ErrContextOverflow` 触发上下文收缩后重试一次。
- 流式增量不入库（见 ADR-0005），装饰器因此不需要关心事件语义，只做透传。
- 加第三家供应商 = 新写一个 `Provider` 实现 + 一套映射测试，不改装饰器与 agent。
