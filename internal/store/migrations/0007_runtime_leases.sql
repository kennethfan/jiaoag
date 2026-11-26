-- 多运行时：任务归属与租约，以及运行时注册表。
--
-- 同一份数据库可以被多个进程同时使用（内嵌 TUI 与 serve 并行）。调度状态
-- 不能只活在各自的内存里，否则两边会各自捞起同一条 queued 任务。owner 与
-- lease_until 让「谁在执行这个任务」可跨进程判定；runtimes 让「谁还活着」
-- 可判定，进程崩溃后由别人接管它的租约。
ALTER TABLE tasks ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS runtimes (
    id           TEXT PRIMARY KEY,
    pid          INTEGER NOT NULL,
    host         TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runtimes_heartbeat ON runtimes(heartbeat_at);
