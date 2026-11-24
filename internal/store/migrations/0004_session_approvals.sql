-- allow_session 授权的持久化（ADR-0030）。
--
-- 一次 allow_session 只对「同一会话 + 同一工具 + 同一段参数原文」生效，
-- 并且带过期时间：它是便利，不是永久豁免。放在库里而不是进程内，是为了
-- 服务端重启后不必把已经同意过的操作再问一遍。
CREATE TABLE IF NOT EXISTS session_approvals (
    session_id TEXT NOT NULL,
    key        TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY (session_id, key)
);

-- 过期清理按时间扫，量很小，但索引让「删除已过期」不必全表。
CREATE INDEX IF NOT EXISTS idx_session_approvals_expires ON session_approvals(expires_at);
