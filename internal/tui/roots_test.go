package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

func TestRootsCommandListsAdditionalRoots(t *testing.T) {
	m := newModel(t)
	m.opts.Roots = []protocol.SessionRootPayload{{Alias: "lib", Path: "/tmp/lib"}}

	m.input = "/roots"
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	joined := strings.Join(m.dialog, "\n")
	if !strings.Contains(joined, "附加根 1 个") || !strings.Contains(joined, "lib → /tmp/lib") {
		t.Fatalf("dialog 未列出附加根：%q", joined)
	}
}

func TestRootsCommandWithoutRoots(t *testing.T) {
	m := newModel(t)
	m.input = "/roots"
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	joined := strings.Join(m.dialog, "\n")
	if !strings.Contains(joined, "-root") {
		t.Fatalf("应提示用 -root 挂载：%q", joined)
	}
}
