package domain

import "errors"

// ErrNotFound 表示按标识查询的记录不存在。
// 存储层用 %w 包装它，调用方用 errors.Is 判断，不比较错误字符串。
var ErrNotFound = errors.New("domain: not found")

// ErrConflict 表示唯一约束冲突（例如同一会话内重复的 seq）。
var ErrConflict = errors.New("domain: conflict")

// ErrSessionBusy 表示会话还有未终态的任务，此刻不能删除：删掉会话会让
// 正在执行的 agent 失去归属，事件也没处可写。先取消或等它结束。
var ErrSessionBusy = errors.New("domain: 会话还有未结束的任务")
