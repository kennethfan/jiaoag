-- 单次任务的模型覆盖（C1）。
--
-- 空串表示跟随会话设置：不用 NULL，是为了让「老任务」与「没覆盖」在读取时
-- 行为一致，也省掉 Scan 时的指针判空。
ALTER TABLE tasks ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';