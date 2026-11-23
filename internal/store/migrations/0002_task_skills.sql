-- 0002_task_skills：任务记住用户显式点名的技能。
--
-- 存 JSON 数组文本（空串表示没有）。之所以不用单独的关联表：技能名只是一组
-- 字符串，查询永远是「按任务读全部」，没有反向索引的需求；项目级技能目录本来
-- 也不进库（技能本体在文件系统里，这里只记名字）。

ALTER TABLE tasks ADD COLUMN skills TEXT NOT NULL DEFAULT '';
