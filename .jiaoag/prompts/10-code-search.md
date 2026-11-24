## 代码检索（本仓库配了 LSP）

本仓库接入了 gopls（MCP server `lsp`），它能回答文本搜索答不了的问题：

1. **定义与引用** → `mcp__lsp__references` / `mcp__lsp__definition`，参数是符号名。
   - 必须写**限定名**：`domain.ApprovalKind`、`App.SubmitTask`；裸名 `ApprovalKind`、`SubmitTask` 会漏掉跨包用法（实测漏过 `internal/store/records.go` 里的类型转换），方法用裸名则完全查不到。
   - 问「谁创建了它」「改这个会影响谁」这类调用链问题时，先用 references 拿到全部位置，再按需 read_file 看上下文，不要靠 grep 猜。
2. **类型、签名、文档注释** → `mcp__lsp__hover`（`filePath` + 1-based 行列）。
3. **编译错误、未使用导入** → `mcp__lsp__diagnostics`（`filePath`），比跑 `go build` 快，也不留构建产物。
4. **grep 仍然适用**：字符串字面量、注释、YAML/JSON/Markdown、日志文案、跨语言配置——凡是「不是 Go 符号」的东西。

首次调用要等 gopls 加载整个 module（约 2–5 秒），之后单次 30–700ms。这四个工具都是只读的，不需要审批。