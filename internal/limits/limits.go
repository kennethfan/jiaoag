// Package limits 收拢跨层的硬上限。
//
// 这些值必须只有一处定义：单条 diff 与单条工具输出用同一个上限，才能保证
// 一个事件既装得下 diff、又不会把事件日志和界面撑爆。分别写在 patch 与
// tools 里改一处忘一处，就会悄悄漂移——而这两个数一旦不等，
// 「diff 装得下但结果装不下」这种边界就只有线上才看得到。
package limits

// MaxPayloadBytes 是单条 diff / 单条工具输出的硬上限（256 KB）。
const MaxPayloadBytes = 256 << 10
