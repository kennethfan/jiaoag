-- approvals.hunks 记录「只批了哪几块」。
--
-- 裁决以前只活在等待协程的内存里，进程一换就丢；现在 approvals 表是唯一
-- 事实来源，另一个运行时读到的必须是同一份决定（含选择的块）。
ALTER TABLE approvals ADD COLUMN hunks TEXT NOT NULL DEFAULT '';