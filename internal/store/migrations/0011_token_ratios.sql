-- 上下文预算估算系数的跨任务记忆（ADR-0057）。
--
-- 估算器本身只在任务内存在；把「实际/估算」的系数按 provider/model 存下来，
-- 新任务首轮就能用上历史校准，不必从 1.0 冷启动（中文与代码尤其吃亏）。
-- 键是 `provider/model`：不同模型的分词器差异比同一供应商内不同版本还大。
CREATE TABLE IF NOT EXISTS token_ratios (
    key        TEXT PRIMARY KEY,
    ratio      REAL NOT NULL,
    samples    INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
