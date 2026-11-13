// Package perm 是权限引擎的最小版本：只按工具的副作用分级。
//
// v1 的安全边界就在这里。规则保持「默认拒绝」：只有明确知道安全的形状才
// 自动放行，其余一律交给人。M5 会在这里加命令白名单，但那是**增加允许**，
// 默认分支仍然是拒绝。
//
// 这里不放策略逻辑以外的东西：数据（名单、风险等级）将来由 Loader 从配置
// 读入（ADR-0010），本包只负责判定顺序。
package perm

import (
	"strings"

	"github.com/kennethfan/jiaoag/internal/tools"
)

// Verdict 是一次判定的结论。
type Verdict string

const (
	// Allow 表示策略自动放行，无需打扰用户（仍要入审计）。
	Allow Verdict = "allow"
	// RequireUser 表示必须由用户裁决。
	RequireUser Verdict = "require_user"
)

// Decision 是一次判定的完整结果。
type Decision struct {
	Verdict Verdict
	// Reason 写进审计与界面，说明「为什么放行/为什么要问」。
	Reason string
	// PolicyVersion 与 RuleID 进审计：事后要能回答「当时用的是哪套规则」。
	PolicyVersion string
	RuleID        string
}

// Decide 按工具声明的副作用给出判定。
//
// 判定顺序（短路）：
//  1. 声明了 propose_write → 必审批：这是唯一会改磁盘的路径；
//  2. 只有 read 副作用 → 放行：读不到东西也伤不到什么，不值得打断用户；
//  3. 其余（process / network / 未声明）→ 必审批：不认识的副作用按最危险的算。
func Decide(spec tools.Spec) Decision {
	writes, onlyRead := effectsClass(spec)
	switch {
	case writes:
		return Decision{Verdict: RequireUser, Reason: "tool_writes: 写入必须经用户批准"}
	case onlyRead:
		return Decision{Verdict: Allow, Reason: "read_only: 只读工具自动放行"}
	default:
		return Decision{Verdict: RequireUser, Reason: "unknown_effect: 未识别的副作用按最危险的算"}
	}
}

// effectsClass 把副作用分成「写」与「纯只读」两类；其余一律不算只读。
func effectsClass(spec tools.Spec) (writes, onlyRead bool) {
	onlyRead = len(spec.Effects) > 0
	for _, e := range spec.Effects {
		switch e {
		case tools.EffectProposeWrite:
			writes = true
		case tools.EffectRead:
		default:
			onlyRead = false
		}
	}
	return writes, onlyRead
}

// Describe 把副作用列表拼成一句人话，供审计与 tool.prepared 事件使用。
func Describe(spec tools.Spec) string {
	out := make([]string, 0, len(spec.Effects))
	for _, e := range spec.Effects {
		out = append(out, string(e))
	}
	return strings.Join(out, ",")
}
