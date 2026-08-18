package version

import "fmt"

var (
	// Version is the current semver release of BroCode.
	Version = "v0.1.1"
	// Commit is the git commit hash, populated at build time via -ldflags.
	Commit = "none"
	// Date is the build timestamp, populated at build time via -ldflags.
	Date = "unknown"
)

// Logo is the compact ASCII brand mark as provided by the user (verbatim,
// character-for-character — kept exact even where the letterforms are
// unconventional). Single source of truth: shared by `brocode -v` and the
// TUI's fresh-session hero banner so the brand never drifts between them.
const Logo = `
┌┐ ┬─┐┌─┐╔═╗┌─┐┌┬┐┌─┐
├┴┐├┬┘│ │║  │ │ ││├┤
└─┘┴└─└─┘╚═╝└─┘─┴┘└─┘`

// Tagline is the CLI's one-line motto.
const Tagline = "ship less, ship right"

// Banner returns the logo plus tagline (used by `brocode -v`).
func Banner() string {
	return Logo + "\n  " + Tagline
}

// Info returns the formatted version information for CLI output.
func Info() string {
	if Commit != "none" && Commit != "" {
		return fmt.Sprintf("BroCode %s (commit %s, built %s)", Version, Commit, Date)
	}
	return fmt.Sprintf("BroCode %s", Version)
}
