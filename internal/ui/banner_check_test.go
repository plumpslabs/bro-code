package ui

import (
	"strings"
	"testing"

	"github.com/plumpslabs/bro-code/internal/version"
)

// TestWelcomeBannerContent pins the fresh-session hero banner: every logo row
// (the shared version.Logo, exact characters), the tagline, the version line,
// and the /help hint. The banner is a plain log message, so it scrolls up with
// the conversation and never overlaps the input — and only appears when a
// session starts with no history.
func TestWelcomeBannerContent(t *testing.T) {
	b := welcomeBanner()
	for _, row := range strings.Split(version.Logo, "\n") {
		if !strings.Contains(b, row) {
			t.Errorf("banner missing logo row %q", row)
		}
	}
	if !strings.Contains(b, version.Tagline) {
		t.Errorf("banner missing tagline %q", version.Tagline)
	}
	if !strings.Contains(b, version.Version) {
		t.Errorf("banner missing version %q", version.Version)
	}
	if !strings.Contains(b, "/help") {
		t.Error("banner missing /help hint")
	}
}
