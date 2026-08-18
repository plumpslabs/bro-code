package version

import "fmt"

var (
	// Version is the current semver release of BroCode.
	Version = "v0.1.0"
	// Commit is the git commit hash, populated at build time via -ldflags.
	Commit = "none"
	// Date is the build timestamp, populated at build time via -ldflags.
	Date = "unknown"
)

// Info returns the formatted version information for CLI output.
func Info() string {
	if Commit != "none" && Commit != "" {
		return fmt.Sprintf("BroCode %s (commit %s, built %s)", Version, Commit, Date)
	}
	return fmt.Sprintf("BroCode %s", Version)
}
