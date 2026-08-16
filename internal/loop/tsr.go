package loop

import (
	"regexp"
	"strings"
)

// maxTSRAttempts caps how many repair cycles the loop will accept before
// giving up gracefully (the typed revision contract's max_attempts).
const maxTSRAttempts = 4

// bugFixSignals are lowercase substrings that suggest the task is a bug fix —
// something is failing and needs to be made to work. Deliberately
// conservative: arming the REPRODUCE gate on a pure feature request would nag
// the model into a pointless reproduce.
var bugFixSignals = []string{
	"error", "exception", "panic", "traceback", "stack trace",
	"fail", "failed", "failing", "failure", "bug", "buggy", "broken",
	"crash", "crashes", "crashing", "segfault", "null pointer",
	"not working", "doesn't work", "does not work", "isn't working", "is not working",
	"gagal", "error", "tidak jalan", "tidak berfungsi", "tidak bekerja", "ngehang", "nggak jalan",
	"regression", "hang", "hangs", "freeze", "freezes", "deadlock", "race",
	"leak", "leaks", "panic:",
}

// providedReproSignals are patterns that indicate the user already supplied a
// reproduction (error message, stack trace, failing output) in the prompt, so
// the REPRODUCE gate is considered satisfied without a run.
var (
	stackTraceRe = regexp.MustCompile(`(?m)(\.go:\d+|\.py:\d+|\.tsx?:\d+|\.rs:\d+|\.java:\d+):?\d*`)
	panicRe      = regexp.MustCompile(`(?i)\bpanic:|Traceback \(most recent call last\)|exit status \d|non-zero exit code|Process finished with exit code \d`)
)

// failureMarkers are substrings that mark a tool output as a reproduced
// failure. Only REAL tool outputs are scanned (guard/deny messages never are),
// so these can be broad.
var failureMarkers = []string{
	"Command failed with error",
	"Command timed out",
	"exit status",
	"exit code",
	"FAIL",
	"--- FAIL:",
	"panic:",
	"Error:",
	"error:",
	"Traceback",
	"Test failed",
	"tests failed",
	"AssertionError",
	"Expected:",
	"Build FAILED",
	"Compilation failed",
	"Type error",
	"Unhandled exception",
	"non-zero",
	"fatal:",
}

// looksLikeBugFixTask reports whether the user query signals a task about a
// bug/failure that should be reproduced before fixing. Armed gates code edits
// behind a TSR REPRODUCE reminder.
func looksLikeBugFixTask(query string) bool {
	if query == "" {
		return false
	}
	lower := strings.ToLower(query)
	for _, sig := range bugFixSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// looksLikeProvidedRepro reports whether the user prompt already contains an
// error message / stack trace / failing output — i.e. the reproduction is
// provided, so the REPRODUCE gate is satisfied without running anything.
func looksLikeProvidedRepro(query string) bool {
	if query == "" {
		return false
	}
	if panicRe.MatchString(query) {
		return true
	}
	// A code-path reference (file:line) near an error keyword is a good signal
	// the user pasted a stack trace.
	if stackTraceRe.MatchString(query) && (strings.Contains(strings.ToLower(query), "error") ||
		strings.Contains(strings.ToLower(query), "panic") ||
		strings.Contains(strings.ToLower(query), "at ")) {
		return true
	}
	return false
}

// looksLikeFailure reports whether a real tool output shows a command/test
// that failed — a reproduction of the bug. Guard/deny/override messages are
// never passed here (only exec'd tool outputs are).
func looksLikeFailure(output string) bool {
	if output == "" {
		return false
	}
	for _, m := range failureMarkers {
		if strings.Contains(output, m) {
			return true
		}
	}
	return false
}

// verifyErrorSignature normalizes a verification failure into a short stable
// signature so the engine can detect "the same error persisting across
// repairs". It takes the FIRST content-bearing line — the actual error message
// — and strips indentation and line numbers, so cosmetic churn (different
// source line, extra compiler context) never resets the streak.
func verifyErrorSignature(errText string) string {
	if strings.TrimSpace(errText) == "" {
		return ""
	}
	for line := range strings.SplitSeq(errText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '.' || line[0] == '/' {
			continue
		}
		sig := line
		if len(sig) > 200 {
			sig = sig[:200]
		}
		return sig
	}
	return ""
}
