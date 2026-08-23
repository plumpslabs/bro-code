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
	tournMsg := "TOURNAMENT:\nFix pooling\n---\n### 🥊 Candidate-Alpha (Minimal Surgical Fix)\nTarget: pool.go:L45\nProposed Patch: add mutex.Lock()\n\n---\n\n### 🥊 Candidate-Beta (Defensive Robust Refactor)\nTarget: pool.go:L10-L80\nProposed Patch: implement ChannelPool wrapper\n\n---\n\n### ⚖️ ARBITER DECISION MATRIX"
	m.Update(tournamentResultMsg(tournMsg))
	if m.status != "Ready" {
		t.Errorf("expected status 'Ready', got %q", m.status)
	}

	// 5. Test resolveTournamentSelection
	enhancedAlpha := resolveTournamentSelection("Apply Alpha", m.messages)
	if !strings.Contains(enhancedAlpha, "Candidate-Alpha") || !strings.Contains(enhancedAlpha, "mutex.Lock()") {
		t.Fatalf("expected enhanced prompt for Candidate-Alpha, got: %s", enhancedAlpha)
	}

	enhancedBeta := resolveTournamentSelection("Apply Beta", m.messages)
	if !strings.Contains(enhancedBeta, "Candidate-Beta") || !strings.Contains(enhancedBeta, "ChannelPool wrapper") {
		t.Fatalf("expected enhanced prompt for Candidate-Beta, got: %s", enhancedBeta)
	}

	// 6. Test extractRecentSessionContext
	recentCtx := extractRecentSessionContext(m.messages, 5)
	if !strings.Contains(recentCtx, "Fix pooling") {
		t.Fatalf("expected recent session context to contain tournament topic, got: %s", recentCtx)
	}
}
