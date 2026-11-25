package tui

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/domain"
)

func TestAutoWriteShowsScopeInDialog(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"workspace", "自动应用（工作区内）：b.txt"},
		{"tmp", "自动应用（临时目录内）：b.txt"},
		{"write_approval=off", "自动应用（按配置免审）：b.txt"},
	} {
		m := newModel(t)
		m.handleEnvelope(eventFrame(t, m.sessionID, string(domain.EventProposalApplied),
			domain.ProposalAppliedPayload{Path: "b.txt", Auto: true, Mode: tc.mode}))
		if !strings.Contains(strings.Join(m.dialog, "\n"), tc.want) {
			t.Fatalf("mode=%s 的提示不对：%v", tc.mode, m.dialog)
		}
	}
}

func TestManualWriteStillSaysWritten(t *testing.T) {
	m := newModel(t)
	m.handleEnvelope(eventFrame(t, m.sessionID, string(domain.EventProposalApplied),
		domain.ProposalAppliedPayload{Path: "b.txt"}))
	got := strings.Join(m.dialog, "\n")
	if !strings.Contains(got, "已写入 b.txt") || strings.Contains(got, "自动应用") {
		t.Fatalf("人工批准后的提示不对：%v", m.dialog)
	}
}
