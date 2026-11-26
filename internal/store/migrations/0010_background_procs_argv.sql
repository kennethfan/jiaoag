-- argv 让 /ps 面板能显示「这条后台进程到底是什么命令」；之前只有 output_path，
-- 属主失联后根本看不出它是 dev server 还是别的。
ALTER TABLE background_procs ADD COLUMN argv TEXT NOT NULL DEFAULT '';
