package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tool-output truncate-and-pointer: long tool results (bash test logs, stack
// traces, lsp scans) are written IN FULL to .brocode/artifacts/ and only a
// head+tail digest with a pointer to the file enters context. The model gets
// the error context it needs (head + tail) without the token bloat of the full
// dump, and can read_file the artifact on demand if it needs details.
//
// The artifacts dir holds at most one turn's logs: cleanupArtifacts wipes it at
// turn start, so the store is bounded by construction (PHILOSOPHY Principles
// 1 + 5 — every persistent store has a lifetime).
const (
	// artifactMaxLines: outputs longer than this (or artifactMaxBytes) spill
	// to disk. 120 lines ≈ a focused test run; a 500-line stack trace is far
	// past it.
	artifactMaxLines = 120
	artifactMaxBytes = 12000
	// artifactHeadTail lines preserved from each end of a truncated output.
	artifactHeadTail = 40
)

// artifactDir returns the turn-scoped artifacts directory under the repo.
func (e *Engine) artifactDir() string {
	if e.repoRoot == "" {
		return ""
	}
	return filepath.Join(e.repoRoot, ".brocode", "artifacts")
}

// cleanupArtifacts wipes the artifacts dir (previous turns' logs). Called at
// turn start so each turn starts with a clean, bounded store.
func (e *Engine) cleanupArtifacts() {
	if dir := e.artifactDir(); dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// capToolOutput applies truncate-and-pointer to a tool result: source reads
// (read_file/edit_file/write_file) are what the model asked for and are never
// truncated; everything else over the threshold spills to an artifact file and
// context receives a head+tail digest with the artifact path. When no repo
// root is available (headless/empty workspace), it degrades to a plain
// head+tail truncation with no file.
func (e *Engine) capToolOutput(toolName, out string) string {
	if out == "" {
		return out
	}
	if toolName == "read_file" || toolName == "edit_file" || toolName == "write_file" {
		return out
	}
	if len(out) <= artifactMaxBytes {
		lines := strings.Count(out, "\n") + 1
		if lines <= artifactMaxLines {
			return out
		}
	}
	lines := strings.Count(out, "\n") + 1

	dir := e.artifactDir()
	if dir != "" {
		e.artifactSeq++
		rel := fmt.Sprintf(".brocode/artifacts/%s-%d.log", toolName, e.artifactSeq)
		abs := filepath.Join(e.repoRoot, rel)
		if os.MkdirAll(filepath.Dir(abs), 0o755) == nil && os.WriteFile(abs, []byte(out), 0o644) == nil {
			head, tail := trimEnds(out, artifactHeadTail)
			return fmt.Sprintf("%s\n\n[output truncated: %d lines — FULL output saved to %s (read_file it if you need the details)]\n\n%s", head, lines, rel, tail)
		}
	}
	// No writable artifact dir (or write failed): keep head+tail inline so the
	// error context is not lost entirely.
	head, tail := trimEnds(out, artifactHeadTail)
	return fmt.Sprintf("%s\n\n[output truncated: %d lines — full output not persisted]\n\n%s", head, lines, tail)
}

// trimEnds returns the first n and last n lines of a multi-line string.
func trimEnds(s string, n int) (head, tail string) {
	lines := strings.Split(s, "\n")
	if len(lines) <= 2*n {
		return s, ""
	}
	return strings.Join(lines[:n], "\n"), strings.Join(lines[len(lines)-n:], "\n")
}
