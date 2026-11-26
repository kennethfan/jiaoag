-- 后台进程的跨运行时可观测与停止意图。
--
-- 进程句柄仍归启动它的运行时所有：别的运行时只能把 stop_requested 置 1，
-- 由属主轮询后执行真正的 kill。status 里的 orphaned 表示属主失联、进程可能
-- 还在跑，但不能再由我们替它收尸（pid 可能已被系统复用）。
CREATE TABLE IF NOT EXISTS background_procs (
    id             TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL,
    owner          TEXT NOT NULL DEFAULT '',
    pid            INTEGER NOT NULL DEFAULT 0,
    output_path    TEXT NOT NULL DEFAULT '',
    started_at     INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'running',
    stop_requested INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_background_procs_session ON background_procs(session_id);
