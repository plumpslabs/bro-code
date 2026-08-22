package ui

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Multi-channel Webhook Dispatcher", "multi_channel_webhook_dispatcher"},
		{"Fix race condition in connection pool!", "fix_race_condition_in_connection_pool"},
		{"", "feature_spec"},
		{"   ---   ", "feature_spec"},
	}

	for _, c := range cases {
		got := slugify(c.in)
		if got != c.want {
			t.Errorf("slugify(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestSpecAndTournamentMessageHandlers(t *testing.T) {
	m := newTestApp()
	m.width = 120
	m.height = 40

	// 1. Test empty /spec
	m.handleSlashCommand("/spec")
	lastMsg := m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "Usage: `/spec <feature description>`") {
		t.Fatalf("expected usage message for empty /spec, got: %s", lastMsg)
	}

	// 2. Test empty /tournament
	m.handleSlashCommand("/tournament")
	lastMsg = m.messages[len(m.messages)-1]
	if !strings.Contains(lastMsg, "Usage: `/tournament <bug or complex task>`") {
		t.Fatalf("expected usage message for empty /tournament, got: %s", lastMsg)
	}

	// 3. Test specResultMsg
	specMsg := "SPEC:\n.brocode/specs/2026-08-22_auth.md\n---\n## 🎯 1. Objective\nCreate authentication system."
	m.Update(specResultMsg(specMsg))
	if m.status != "Ready" {
		t.Errorf("expected status 'Ready', got %q", m.status)
	}

	// 4. Test tournamentResultMsg
	tournMsg := "TOURNAMENT:\nFix pooling\n---\n### 🥊 Candidate-Alpha\nPassed all tests with 2 line diff."
	m.Update(tournamentResultMsg(tournMsg))
	if m.status != "Ready" {
		t.Errorf("expected status 'Ready', got %q", m.status)
	}
}
