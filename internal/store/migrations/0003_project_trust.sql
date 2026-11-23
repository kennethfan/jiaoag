-- 项目级扩展的信任记录（ADR-0025）。
--
-- 键是工作区绝对路径，值是被确认过的内容摘要。摘要不匹配就说明仓库里的
-- 提示/技能/MCP 清单变了，需要重新确认。记录放在数据目录而不是仓库，避免
-- 被 git 带进别人的机器：信任必须由本人给出。
CREATE TABLE IF NOT EXISTS project_trust (
    workspace  TEXT PRIMARY KEY,
    digest     TEXT NOT NULL,
    trusted_at TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT ''
);