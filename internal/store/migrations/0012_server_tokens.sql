-- 访问令牌落库：配置文件只在库为空时做一次种子导入，之后以库为准。
--
-- 只存 sha256（hex），不存明文：令牌泄露通常是从备份、日志或数据库副本里
-- 捞出来的，库里没有明文能少一个泄口。明文只在 `jiaoag admin token
-- create/rotate` 的输出里出现一次。
CREATE TABLE IF NOT EXISTS server_tokens (
    name       TEXT PRIMARY KEY,
    hash       TEXT NOT NULL,
    read_only  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    rotated_at TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);

-- 版本号：服务端每 2 秒看一次，变了就重载。这样 Windows 上不需要 SIGHUP
-- 也能热更新（SIGHUP 仍保留为「立即重读」）。
CREATE TABLE IF NOT EXISTS server_token_meta (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    version INTEGER NOT NULL
);

INSERT OR IGNORE INTO server_token_meta (id, version) VALUES (1, 1);