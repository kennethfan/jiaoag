-- 0001_init: 事件日志 + 投影的初始结构。
-- 时间列统一为 UTC RFC3339Nano 文本，零值存空串。

CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    workspace   TEXT NOT NULL,
    provider    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE tasks (
    id                 TEXT PRIMARY KEY,
    session_id         TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    status             TEXT NOT NULL,
    prompt             TEXT NOT NULL DEFAULT '',
    system_prompt_hash TEXT NOT NULL DEFAULT '',
    extensions_digest  TEXT NOT NULL DEFAULT '',
    last_seq           INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL,
    started_at         TEXT NOT NULL DEFAULT '',
    finished_at        TEXT NOT NULL DEFAULT '',
    error              TEXT NOT NULL DEFAULT '',
    prompt_tokens      INTEGER NOT NULL DEFAULT 0,
    completion_tokens  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_tasks_session ON tasks(session_id, created_at);

-- 事件只追加。seq 由会话内分配，允许空洞（流式增量不入库）。
CREATE TABLE events (
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    v           INTEGER NOT NULL DEFAULT 1,
    type        TEXT NOT NULL,
    payload     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    task_id     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, seq)
);

CREATE TABLE tool_calls (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    round       INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL,
    args        TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    approval_id TEXT NOT NULL DEFAULT '',
    output_path TEXT NOT NULL DEFAULT '',
    exit_code   INTEGER,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    ended_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_tool_calls_task ON tool_calls(task_id);

CREATE TABLE approvals (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    risk        TEXT NOT NULL DEFAULT '',
    decision    TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL DEFAULT '',
    requested_at TEXT NOT NULL,
    resolved_at TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE proposals (
    id           TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    path         TEXT NOT NULL,
    diff         TEXT NOT NULL DEFAULT '',
    base_hash    TEXT NOT NULL DEFAULT '',
    new_hash     TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    applied_at   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE audit (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           TEXT NOT NULL,
    actor        TEXT NOT NULL DEFAULT '',
    action       TEXT NOT NULL,
    tool_call_id TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_audit_tool_call ON audit(tool_call_id);
