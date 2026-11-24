-- 会话的只读附加根（ADR-0036）。用 JSON 文本存 [{alias,path}]：
-- 附加根是会话属性，不参与查询与关联，单列足够。
ALTER TABLE sessions ADD COLUMN roots TEXT NOT NULL DEFAULT '';
