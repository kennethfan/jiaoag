package perm_test

import (
	"testing"

	"github.com/kennethfan/jiaoag/internal/perm"
	"github.com/kennethfan/jiaoag/internal/tools"
)

func TestDecideReadOnlyIsAllowed(t *testing.T) {
	got := perm.Decide(tools.Spec{Name: "read_file", Effects: []tools.Effect{tools.EffectRead}})
	if got.Verdict != perm.Allow {
		t.Fatalf("只读工具应放行，实际 %q（%s）", got.Verdict, got.Reason)
	}
}

func TestDecideWriteRequiresUser(t *testing.T) {
	for _, effects := range [][]tools.Effect{
		{tools.EffectProposeWrite},
		{tools.EffectRead, tools.EffectProposeWrite},
	} {
		got := perm.Decide(tools.Spec{Name: "write_file", Effects: effects})
		if got.Verdict != perm.RequireUser {
			t.Fatalf("副作用 %v 应要求审批，实际 %q", effects, got.Verdict)
		}
	}
}

func TestDecideUnknownEffectRequiresUser(t *testing.T) {
	cases := [][]tools.Effect{
		nil,
		{},
		{tools.EffectProcess},
		{tools.EffectNetwork},
		{tools.EffectRead, tools.EffectProcess},
	}
	for _, effects := range cases {
		got := perm.Decide(tools.Spec{Name: "x", Effects: effects})
		if got.Verdict != perm.RequireUser {
			t.Fatalf("副作用 %v 应默认拒绝，实际 %q", effects, got.Verdict)
		}
	}
}

func TestDescribeJoinsEffects(t *testing.T) {
	got := perm.Describe(tools.Spec{Effects: []tools.Effect{tools.EffectRead, tools.EffectProposeWrite}})
	if got != "read,propose_write" {
		t.Fatalf("Describe = %q", got)
	}
	if perm.Describe(tools.Spec{}) != "" {
		t.Fatal("没有副作用时应为空串")
	}
}
