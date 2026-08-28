package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// Convention checks are deterministic, language-aware code-quality guards
// that run natively after the model edits files (no LLM involved). They catch
// the mistakes real developers make daily that a build alone won't:
//
//   - leftover debug statements (console.log / debugger / fmt.Println / print)
//   - leftover TODO/FIXME/HACK markers in freshly written code
//   - type-safety red flags (TS `any`, Go `interface{}` written as a value)
//   - duplicate symbol definitions (reimplementing something that already
//     exists instead of reusing it)
//
// Results are fed back into the model's context so it can fix them before
// declaring done — the agentic equivalent of a senior dev's code review.

// Severity levels follow the conventional critical/error/warning/info ladder
// (same semantics as matcha's pattern registry) so the review output tells
// the model which findings are blockers and which are nice-to-have.
type severity int

const (
	sevInfo severity = iota
	sevWarning
	sevError
	sevCritical
)

func (s severity) String() string {
	switch s {
	case sevCritical:
		return "critical"
	case sevError:
		return "error"
	case sevWarning:
		return "warning"
	default:
		return "info"
	}
}

// pattern is one language-aware check: a regex, its severity, and an optional
// context regex whose presence SUPPRESSES the finding (ignoreContext).
type pattern struct {
	kind     string
	sev      severity
	re       *regexp.Regexp
	ignoreRe *regexp.Regexp
}

// patternsByExt maps file extension → checks, mirroring matcha's pattern
// registry (a curated subset chosen for low false-positive rate and high
// signal). Multi-language, deterministic, zero-token.
var patternsByExt = map[string][]pattern{
	".js":    append(jstsShared(), jsShared()...),
	".jsx":   append(jstsShared(), jsShared()...),
	".mjs":   append(jstsShared(), jsShared()...),
	".cjs":   append(jstsShared(), jsShared()...),
	".ts":    append(jstsShared(), tsShared()...),
	".tsx":   append(jstsShared(), tsShared()...),
	".mts":   append(jstsShared(), tsShared()...),
	".cts":   append(jstsShared(), tsShared()...),
	".go":    goPatterns(),
	".py":    pyPatterns(),
	".rs":    rustPatterns(),
	".java":  javaPatterns(),
	".c":     cPatterns(),
	".cpp":   cPatterns(),
	".h":     cPatterns(),
	".hpp":   cPatterns(),
	".cs":    csPatterns(),
	".php":   phpPatterns(),
	".rb":    rbPatterns(),
	".swift": swiftPatterns(),
	".kt":    ktPatterns(),
	".dart":  dartPatterns(),
}

// conventionIssue is one problem found in an edited file.
type conventionIssue struct {
	Path    string
	Line    int
	Kind    string // "debug", "marker", "type-safety", "duplicate", "security", "error-handling", ...
	Sev     severity
	Message string
}

// checkFileConventionsWithOld scans a single file for convention issues, skipping any
// lines that already existed verbatim in oldContent so pre-existing legacy code is never flagged.
func checkFileConventionsWithOld(path string, oldContent string) []conventionIssue {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	ext := strings.ToLower(filepath.Ext(path))
	lines := strings.Split(content, "\n")
	patterns := patternsByExt[ext]

	existingLines := make(map[string]bool)
	if oldContent != "" {
		for _, ol := range strings.Split(oldContent, "\n") {
			existingLines[strings.TrimSpace(ol)] = true
		}
	}

	var issues []conventionIssue
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if existingLines[trimmed] {
			continue // skip pre-existing untouched legacy lines
		}
		for _, p := range patterns {
			if p.ignoreRe != nil && p.ignoreRe.MatchString(line) {
				continue
			}
			if m := p.re.FindString(line); m != "" {
				issues = append(issues, conventionIssue{
					Path:    path,
					Line:    i + 1,
					Kind:    p.kind,
					Sev:     p.sev,
					Message: p.kind + ": " + m,
				})
			}
		}
	}

	// Structural checks: function length, file length, param count, nesting, TODO debt
	issues = append(issues, structuralChecks(path, oldContent, lines)...)

	return issues
}

// checkFileConventions scans a single file for convention issues.
func checkFileConventions(path string) []conventionIssue {
	return checkFileConventionsWithOld(path, "")
}

// ── language check builders (curated subset of matcha's registry) ────────

func jstsShared() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`console\.(log|debug|warn)\([^;]*\)`), ignoreRe: regexp.MustCompile(`(_test\.|\bspec\.|test\(|describe\(|it\()`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "empty-catch", sev: sevError, re: regexp.MustCompile(`catch\s*(\(\s*\w*\s*\))?\s*\{\s*\}`)},
		{kind: "hardcoded-secret", sev: sevCritical, re: regexp.MustCompile(`(?i)(?:api[_-]?key|secret|password|passwd|token)\s*[:=]\s*["'][A-Za-z0-9_\-+/=]{8,}["']`), ignoreRe: regexp.MustCompile(`process\.env|import\.meta\.env|\bENV\[|getenv|\{\{|\{\%`)},
		{kind: "insecure-random", sev: sevWarning, re: regexp.MustCompile(`Math\.random\(\)`), ignoreRe: regexp.MustCompile(`shuffle|animation|jitter|mock|test`)},
		{kind: "loose-equality", sev: sevInfo, re: regexp.MustCompile(`[^=!]==[^=]|[^=!]!=[^=]`)},
		{kind: "sql-injection", sev: sevCritical, re: regexp.MustCompile(`(?:SELECT|INSERT|UPDATE|DELETE)\s[^;\"']*\"\s*\+\s*`), ignoreRe: regexp.MustCompile(`\?\s*\"|:\w+\"|\$\{\w+\}\"`)},
	}
}

func jsShared() []pattern {
	return []pattern{
		{kind: "debugger", sev: sevWarning, re: regexp.MustCompile(`\bdebugger\b`)},
	}
}

func tsShared() []pattern {
	return []pattern{
		{kind: "type-safety", sev: sevWarning, re: regexp.MustCompile(`:\s*any\b|as\s+any\b`)},
		{kind: "type-safety", sev: sevWarning, re: regexp.MustCompile(`@ts-ignore|@ts-nocheck`)},
		{kind: "type-safety", sev: sevInfo, re: regexp.MustCompile(`\w!\s*[.\[;,\)]`), ignoreRe: regexp.MustCompile(`_test\.|\bspec\.`)},
	}
}

func goPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`fmt\.(Println|Printf|Print)\(|log\.Print(ln|f)?\(`), ignoreRe: regexp.MustCompile(`_test\.go$|func main\s*\(`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "swallowed-error", sev: sevError, re: regexp.MustCompile(`if\s+err\s*!=\s*nil\s*\{\s*\}`)},
		{kind: "ignored-error", sev: sevWarning, re: regexp.MustCompile(`\b_\s*=\s*\w+\([^)]*\)\s*(//.*)?$`)},
		{kind: "panic-in-lib", sev: sevWarning, re: regexp.MustCompile(`\bpanic\(`), ignoreRe: regexp.MustCompile(`_test\.go$|func main\s*\(`)},
		{kind: "hardcoded-secret", sev: sevCritical, re: regexp.MustCompile(`(?:apiKey|secret|password|passwd|token)\s*:?=\s*"[A-Za-z0-9_\-+/=]{8,}"`), ignoreRe: regexp.MustCompile(`os\.Getenv|viper\.|flag\.`)},
		{kind: "insecure-random", sev: sevWarning, re: regexp.MustCompile(`math/rand`), ignoreRe: regexp.MustCompile(`crypto/rand`)},
	}
}

func pyPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`(?m)^\s*print\(|\bpprint\(|breakpoint\(\)|pdb\.set_trace\(`), ignoreRe: regexp.MustCompile(`if\s+__name__\s*==\s*["']__main__["']`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`#\s*(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "empty-catch", sev: sevError, re: regexp.MustCompile(`except[^:]*:\s*\n\s*pass\b|except[^:]*:\s*pass\b`)},
		{kind: "bare-except", sev: sevWarning, re: regexp.MustCompile(`except\s*:`)},
		{kind: "hardcoded-secret", sev: sevCritical, re: regexp.MustCompile(`(?i)(?:api[_-]?key|secret|password|passwd|token)\s*=\s*["'][A-Za-z0-9_\-+/=]{8,}["']`), ignoreRe: regexp.MustCompile(`os\.environ|getenv|os\.getenv`)},
		{kind: "mutable-default", sev: sevWarning, re: regexp.MustCompile(`def\s+\w+\([^)]*=\s*(\[\]|\{\}|dict\(\)|list\(\))`)},
		{kind: "insecure-random", sev: sevWarning, re: regexp.MustCompile(`\brandom\.(random|randint|choice)\(`), ignoreRe: regexp.MustCompile(`secrets\.`)},
		{kind: "sql-injection", sev: sevCritical, re: regexp.MustCompile(`f["'][^"']*\{[^}"]*\}[^"']*\b(SELECT|INSERT|UPDATE|DELETE)\b`)},
	}
}

func rustPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`println!\(|dbg!\(|eprintln!\(`), ignoreRe: regexp.MustCompile(`#\[cfg\(test\)\]|mod tests`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "unwrap-panic", sev: sevWarning, re: regexp.MustCompile(`\.unwrap\(\)`), ignoreRe: regexp.MustCompile(`#\[cfg\(test\)\]|mod tests`)},
		{kind: "unsafe-block", sev: sevWarning, re: regexp.MustCompile(`\bunsafe\s*\{`)},
	}
}

func javaPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`System\.out\.print(ln)?\(|System\.err\.print(ln)?\(`), ignoreRe: regexp.MustCompile(`_test\.|\.test\.`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "empty-catch", sev: sevError, re: regexp.MustCompile(`catch\s*\([^)]+\)\s*\{\s*\}`)},
		{kind: "generic-exception", sev: sevInfo, re: regexp.MustCompile(`catch\s*\(\s*Exception\s+\w+\s*\)|throws\s+Exception\b`)},
	}
}

func cPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`printf\(|std::cout\s*<<|fprintf\(stderr`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "unsafe-memory", sev: sevError, re: regexp.MustCompile(`\bstrcpy\(|\bstrcat\(|\bsprintf\(|\bgets\(`)},
	}
}

func csPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`Console\.Write(Line)?\(|Debug\.WriteLine\(`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "empty-catch", sev: sevError, re: regexp.MustCompile(`catch\s*(\([^)]*\))?\s*\{\s*\}`)},
	}
}

func phpPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`\bvar_dump\(|\bprint_r\(|\bdd\(`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "sql-injection", sev: sevCritical, re: regexp.MustCompile(`(?:SELECT|INSERT|UPDATE|DELETE)\s[^;\"]*\"\s*\.\s*\$`)},
	}
}

func rbPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`\bputs\s|\bp\s|\bpp\s|binding\.pry|\bbyebug\b`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`#\s*(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "empty-catch", sev: sevError, re: regexp.MustCompile(`rescue\s*(=>\s*\w+)?\s*\n\s*end|rescue\s*$`)},
	}
}

func swiftPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`\bprint\(|debugPrint\(|\bdump\(`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "force-unwrap", sev: sevWarning, re: regexp.MustCompile(`\w!\s*[.\n;,\)]`)},
	}
}

func ktPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`println\(|Log\.d\(`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "not-null-assert", sev: sevWarning, re: regexp.MustCompile(`\w!!\s*[.\[;,\)]`)},
	}
}

func dartPatterns() []pattern {
	return []pattern{
		{kind: "debug", sev: sevWarning, re: regexp.MustCompile(`\bprint\(|debugPrint\(`)},
		{kind: "marker", sev: sevInfo, re: regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)},
		{kind: "empty-catch", sev: sevError, re: regexp.MustCompile(`catch\s*\([^)]*\)\s*\{\s*\}`)},
	}
}

var commonSymbolStopwords = map[string]bool{
	"id": true, "err": true, "error": true, "data": true, "req": true, "res": true,
	"result": true, "response": true, "client": true, "config": true, "options": true,
	"state": true, "params": true, "props": true, "event": true, "item": true,
	"index": true, "key": true, "val": true, "value": true, "text": true, "body": true,
	"status": true, "url": true, "msg": true, "message": true, "logger": true,
	"test": true, "main": true, "init": true, "helper": true, "utils": true,
	"handler": true, "service": true, "model": true, "controller": true,
}

// symbolNameRe extracts top-level function/class/interface/type names for duplicate
// detection across major programming languages (JS/TS/Go/Python/Rust/Java/PHP/C#/C++).
var symbolNameRe = map[string]*regexp.Regexp{
	".js":   regexp.MustCompile(`(?m)^(?:export\s+(?:default\s+)?)?(?:async\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*))`),
	".jsx":  regexp.MustCompile(`(?m)^(?:export\s+(?:default\s+)?)?(?:async\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*))`),
	".ts":   regexp.MustCompile(`(?m)^(?:export\s+(?:default\s+)?)?(?:async\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*)|interface\s+([A-Za-z_$][\w$]*)|type\s+([A-Za-z_$][\w$]*))`),
	".tsx":  regexp.MustCompile(`(?m)^(?:export\s+(?:default\s+)?)?(?:async\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*)|interface\s+([A-Za-z_$][\w$]*)|type\s+([A-Za-z_$][\w$]*))`),
	".go":   regexp.MustCompile(`(?m)^func\s+(?:\([^)]+\)\s+)?([A-Za-z_][\w]*)\s*\(`),
	".py":   regexp.MustCompile(`(?m)^(?:async\s+)?def\s+([a-zA-Z_]\w*)\s*\(|^class\s+([A-Za-z_]\w*)\s*:`),
	".rs":   regexp.MustCompile(`(?m)^(?:\s*pub(?:\([^)]*\))?\s+)?(?:fn\s+([A-Za-z_][\w]*)|struct\s+([A-Za-z_][\w]*)|enum\s+([A-Za-z_][\w]*)|trait\s+([A-Za-z_][\w]*))`),
	".java": regexp.MustCompile(`(?m)^\s*(?:public|protected|private)?\s*(?:static\s+)?(?:class\s+([A-Za-z_][\w]*)|interface\s+([A-Za-z_][\w]*)|(?:[A-Za-z_][\w<>\[\]]*\s+)+([A-Za-z_][\w]*)\s*\()`),
	".php":  regexp.MustCompile(`(?m)^\s*(?:final\s+|abstract\s+)?(?:class\s+([A-Za-z_][\w]*)|function\s+([A-Za-z_][\w]*))`),
	".cs":   regexp.MustCompile(`(?m)^\s*(?:public|protected|private|internal)?\s*(?:static\s+)?(?:class\s+([A-Za-z_][\w]*)|interface\s+([A-Za-z_][\w]*))`),
	".cpp":  regexp.MustCompile(`(?m)^\s*(?:class\s+([A-Za-z_][\w]*)|struct\s+([A-Za-z_][\w]*))`),
	".hpp":  regexp.MustCompile(`(?m)^\s*(?:class\s+([A-Za-z_][\w]*)|struct\s+([A-Za-z_][\w]*))`),
}

// symbolsInFile returns the set of top-level symbol names defined in a file.
func symbolsInFile(path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	re := symbolNameRe[ext]
	if re == nil {
		return nil
	}
	syms := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		for _, g := range m[1:] {
			if g != "" && len(g) >= 4 && !commonSymbolStopwords[strings.ToLower(g)] {
				syms[g] = true
			}
		}
	}
	return syms
}

// findDuplicateSymbols scans the given edited files against each other AND
// against a provided global symbol map (from the persistent index). If the
// model wrote a function that already exists elsewhere, that's a reuse miss.
//
// editedPaths are the files the model touched this turn; knownSymbols maps
// file path -> defined symbols from the wider codebase (nil skips the
// cross-file check). The same-file cross-check always runs.
func findDuplicateSymbols(editedPaths []string, knownSymbols map[string]map[string]bool) []conventionIssue {
	var issues []conventionIssue
	// Same-file duplicate check (two functions with the same name in one file).
	for _, p := range editedPaths {
		syms := symbolsInFile(p)
		if len(syms) == 0 {
			continue
		}
		// Cross-file: does this symbol already exist elsewhere?
		if knownSymbols != nil {
			for s := range syms {
				for other, otherSyms := range knownSymbols {
					if other == p {
						continue
					}
					if otherSyms[s] {
						issues = append(issues, conventionIssue{
							Path:    p,
							Kind:    "duplicate",
							Message: "symbol '" + s + "' already defined in " + other + " — prefer reusing the existing implementation",
						})
						if len(issues) >= 5 {
							return issues
						}
						break
					}
				}
			}
		}
	}
	return issues
}

// extractToolPath pulls the "path" argument out of a write_file/edit_file
// arguments JSON, so the engine can track which files the model touched.
func extractToolPath(argsJSON string) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	return args.Path
}

// maxReviewPasses caps how many edit-review rounds run per turn so a model
// that keeps editing in response to review feedback cannot loop forever.
const maxReviewPasses = 2

// diffTouchedLines approximates how many lines an edit added or removed (line
// multiset difference). Used to gate the expensive LLM review layer on edit
// complexity: small diffs get deterministic + one-pass review.
func diffTouchedLines(oldText, newText string) int {
	if oldText == newText {
		return 0
	}
	oldSet := map[string]int{}
	for l := range strings.SplitSeq(oldText, "\n") {
		oldSet[l]++
	}
	newSet := map[string]int{}
	for l := range strings.SplitSeq(newText, "\n") {
		newSet[l]++
	}
	var touched int
	for l, c := range oldSet {
		if d := c - newSet[l]; d > 0 {
			touched += d
		}
	}
	for l, c := range newSet {
		if d := c - oldSet[l]; d > 0 {
			touched += d
		}
	}
	return touched
}

// reviewEditedFiles runs the two-layer review over the files edited this turn:
//   - Layer 1 (free, deterministic): debug leftovers, markers, type-safety red
//     flags, duplicate symbols, plus LSP diagnostics when wired.
//   - Layer 2 (LLM, senior-level): one bounded completion that reviews the
//     edited files for what regex cannot see — N+1/SQL patterns, error
//     handling, concurrency, reuse, security, conventions.
//
// Returns the formatted review block, or "" when everything is clean. The
// two layers are deliberately ordered so the expensive LLM pass only runs
// after the free checks pass (and only on the first review round per turn).
func (e *Engine) reviewEditedFiles(ctx context.Context) string {
	if len(e.editedFiles) == 0 {
		return ""
	}
	e.reviewPasses++
	var issues []conventionIssue

	// Build map of path -> old content to ignore pre-existing legacy issues
	oldContents := make(map[string]string)
	for _, c := range tool.PeekChanges() {
		if _, ok := oldContents[c.Path]; !ok {
			oldContents[c.Path] = c.Old
		}
	}

	for _, p := range e.editedFiles {
		issues = append(issues, checkFileConventionsWithOld(p, oldContents[p])...)
	}
	var knownSyms map[string]map[string]bool
	if e.symbolsProvider != nil {
		knownSyms = e.symbolsProvider()
	}
	issues = append(issues, findDuplicateSymbols(e.editedFiles, knownSyms)...)

	// Complexity gate for the expensive LLM layer: small edits (≤30 lines
	// touched this turn) are high-confidence targets — deterministic checks
	// plus ONE correctness angle is enough; the second security/edge angle is
	// reserved for edits big enough to hide regressions.
	const smallEditLines = 30
	var changedLines int
	for _, c := range tool.PeekChanges() {
		changedLines += diffTouchedLines(c.Old, c.New)
	}
	highComplexity := changedLines > smallEditLines

	// Supervisory Blast-Radius Check: detect unconstrained spread across files
	if len(e.editedFiles) > 4 && changedLines > 250 {
		issues = append(issues, conventionIssue{
			Kind:    "blast-radius",
			Message: fmt.Sprintf("⚠️ [BLAST RADIUS WARNING]: Your changes have touched %d files with %d modified lines. For targeted tasks, do not refactor out-of-scope code or rewrite distant files.", len(e.editedFiles), changedLines),
		})
	}

	// Native type errors via LSP (when wired) — catches what regex can't.
	if e.diagFn != nil {
		for _, p := range e.editedFiles {
			if out := e.diagFn(p); out != "" && !strings.HasPrefix(out, "No diagnostics") {
				issues = append(issues, conventionIssue{
					Path:    p,
					Kind:    "type-error",
					Message: out,
				})
			}
		}
	}

	// Layer 2: Adaptive Verification Ladder (Prompt-Induced Waste Mitigation).
	// When Layer 1 deterministic checks (regex syntax, duplicates, LSP) are clean,
	// run Layer 2 (senior LLM review): correctness angle (round 1).
	// The second angle (security/edge review in round 2) is adaptively reserved
	// for high-complexity tasks (>30 lines touched) to prevent excessive review loops.
	if len(issues) == 0 && e.reviewLLMEnabled && e.reviewPasses <= maxReviewPasses {
		if e.reviewPasses == 1 || highComplexity {
			var angle = llmReviewCorrectness
			if e.reviewPasses > 1 {
				angle = llmReviewSecurity
			}
			if out := e.llmReviewEditedFiles(ctx, angle); out != "" {
				issues = append(issues, conventionIssue{
					Kind:    "senior-review",
					Message: out,
				})
			}
		}
	}

	e.editedFiles = nil
	if len(issues) == 0 {
		return ""
	}
	return formatConventionIssues(issues)
}

// reviewAngle selects which senior-review lens runs. Correctness (round 1)
// covers the classic merge-blockers; Security (round 2) runs only after the
// correctness pass is clean so the SAME code is examined from a second
// perspective — catching issues one lens would gloss over.
type reviewAngle int

const (
	llmReviewCorrectness reviewAngle = iota
	llmReviewSecurity
)

// llmReviewEditedFiles runs one bounded senior-level code review over the
// edited files using the given lens. It returns "" on any failure (never
// blocks the turn) and keeps the prompt + output small so the cost stays
// proportional. The call inherits the turn's context (so ESC interrupts it)
// and is additionally bounded by a 60s timeout — a hung provider must never
// stall the turn (previously it used context.Background, which ignored ESC and
// could block up to the HTTP client's full timeout).
func (e *Engine) llmReviewEditedFiles(ctx context.Context, angle reviewAngle) string {
	files := e.editedFiles
	if len(files) == 0 {
		return ""
	}
	// Cap how much code the reviewer sees: up to 3 files, 120 lines each.
	if len(files) > 3 {
		files = files[:3]
	}
	var sb strings.Builder
	for _, p := range files {
		if diff := tool.CumulativeChangeDiff(p); diff != "" {
			fmt.Fprintf(&sb, "=== DIFF FOR %s ===\n%s\n", p, diff)
		} else {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			lines := strings.Split(string(data), "\n")
			if len(lines) > 120 {
				lines = lines[:120]
			}
			fmt.Fprintf(&sb, "=== %s ===\n%s\n", p, strings.Join(lines, "\n"))
		}
	}
	if sb.Len() == 0 {
		return ""
	}

	var prompt string
	temperature := 0.1
	switch angle {
	case llmReviewSecurity:
		temperature = 0.3
		prompt = `You are a senior security & robustness reviewer. Review ONLY the newly changed code/diff below from a SECOND, different angle — security, edge cases, and regression risk. Do NOT flag pre-existing lines outside the change:
- Security: user input reaching SQL/shell/HTML/deserialization sinks; missing authz/validation on the new code path; secrets logged or committed; unsafe eval/exec of untrusted data
- Edge cases: empty input, nil/zero values, off-by-one, race between check and use (TOCTOU), boundary loops, overflow
- Performance regression: the change introducing quadratic behavior, repeated allocations in loops, blocking calls on hot paths
- Cross-module side effects: the change breaking callers outside these files (signature changes, removed exports, changed return semantics)
Be terse. Format: one line per issue as "file:line — problem → fix". If the changes are clean, reply exactly: CLEAN

` + sb.String()
	default: // llmReviewCorrectness
		prompt = `You are a senior code reviewer. Review ONLY the newly changed code/diff below for real, high-signal problems introduced in this change. Do NOT flag pre-existing lines outside the change:
- N+1 query patterns (DB query inside a loop — flag the loop and suggest batch loading)
- SQL: unbounded SELECT *, missing WHERE on updates/deletes, string-built queries (injection risk)
- Error handling: swallowed errors, empty catch, ignoring return errors
- Concurrency: shared mutable state, races, blocking calls in hot paths
- Reuse: reimplementing something that likely exists
- Security: user input concatenated into SQL/shell/HTML
- Obvious performance traps (quadratic loops, repeated work in loops)
Be terse. Format: one line per issue as "file:line — problem → fix". If the changes are clean, reply exactly: CLEAN

` + sb.String()
	}

	reviewCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req := provider.CompletionRequest{
		Model:       e.model,
		Messages:    []provider.Message{{Role: "user", Content: prompt}},
		Temperature: temperature,
	}
	resp, err := e.complete(reviewCtx, req)
	if err != nil || resp == nil {
		return ""
	}
	out := strings.TrimSpace(resp.Content)
	if out == "" || strings.EqualFold(out, "CLEAN") {
		return ""
	}
	// Cap the review output so it cannot flood the context.
	if len(out) > 800 {
		out = out[:800] + "…"
	}
	return out
}

// formatConventionIssues renders issues as a compact review block for the
// model's context. Severity is shown so the model knows blockers (critical /
// error) must be fixed while info/warning are optional polish.
func formatConventionIssues(issues []conventionIssue) string {
	if len(issues) == 0 {
		return ""
	}
	const maxIssues = 8
	var sb strings.Builder
	sb.WriteString("Code review found issues to fix before done:\n")
	shown := 0
	for _, i := range issues {
		if shown >= maxIssues {
			fmt.Fprintf(&sb, "- ... and %d more minor convention items\n", len(issues)-shown)
			break
		}
		loc := i.Path
		if i.Line > 0 {
			loc += ":" + itoa(i.Line)
		}
		fmt.Fprintf(&sb, "- [%s] %s — %s\n", i.Sev, loc, i.Message)
		shown++
	}
	res := strings.TrimSpace(sb.String())
	if len(res) > 1500 {
		res = res[:1500] + "… (review output capped)"
	}
	return res
}

// ── structural checks (file-level, not per-line regex) ───────────────

const (
	maxFuncLines   = 50  // warn when a function body exceeds this
	maxFileLines   = 300 // warn when total file lines exceed this
	maxParams      = 5   // warn when function signature has too many params
	maxNestingDepth = 4  // warn when nesting exceeds this
	maxTODOsPerFile = 5  // warn when TODO/FIXME count exceeds this
)

// structuralChecks runs file-level analysis that single-line regex cannot
// catch: function length, file length, parameter count, nesting depth,
// and TODO debt concentration. Only flags lines in NEWLY ADDED content
// (lines not in oldContent) to avoid spamming legacy code.
func structuralChecks(path string, oldContent string, lines []string) []conventionIssue {
	ext := strings.ToLower(filepath.Ext(path))
	// Only run structural checks on languages we have function-detection for.
	if ext != ".go" && ext != ".js" && ext != ".ts" && ext != ".jsx" && ext != ".tsx" &&
			ext != ".py" && ext != ".rs" && ext != ".java" && ext != ".cs" &&
			ext != ".php" && ext != ".rb" && ext != ".swift" && ext != ".kt" && ext != ".dart" {
		return nil
	}

	existingLines := make(map[int]bool)
	if oldContent != "" {
		for i, ol := range strings.Split(oldContent, "\n") {
			existingLines[i] = strings.TrimSpace(ol) != ""
		}
	}

	var issues []conventionIssue

	// File length check
	if len(lines) > maxFileLines {
		issues = append(issues, conventionIssue{
			Path:    path,
			Kind:    "file-length",
			Sev:     sevWarning,
			Message: fmt.Sprintf("file has %d lines (threshold %d) — consider splitting into smaller modules", len(lines), maxFileLines),
		})
	}

	// TODO/FIXME debt concentration
	todoCount := 0
	for _, line := range lines {
		if re := regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`); re.MatchString(line) {
			todoCount++
		}
	}
	if todoCount > maxTODOsPerFile {
		issues = append(issues, conventionIssue{
			Path:    path,
			Kind:    "todo-debt",
			Sev:     sevWarning,
			Message: fmt.Sprintf("file has %d TODO/FIXME markers (threshold %d) — prioritize or remove stale markers", todoCount, maxTODOsPerFile),
		})
	}

	// Function-level checks: length, parameter count, nesting depth
	funcRanges := detectFunctions(path, lines)
	for _, fr := range funcRanges {
		// Skip if function body is entirely pre-existing
		newLines := 0
		for i := fr.start; i <= fr.end && i < len(lines); i++ {
			if !existingLines[i] {
				newLines++
		}
		}
		if newLines == 0 {
			continue
		}

		funcLen := fr.end - fr.start + 1
		if funcLen > maxFuncLines {
			issues = append(issues, conventionIssue{
				Path:    path,
				Line:    fr.start + 1,
				Kind:    "function-length",
				Sev:     sevWarning,
				Message: fmt.Sprintf("function '%s' is %d lines (threshold %d) — extract helper functions or split logic", fr.name, funcLen, maxFuncLines),
			})
		}

		if fr.params > maxParams {
			issues = append(issues, conventionIssue{
				Path:    path,
				Line:    fr.start + 1,
				Kind:    "too-many-params",
				Sev:     sevInfo,
				Message: fmt.Sprintf("function '%s' has %d parameters (threshold %d) — group into options/config struct", fr.name, fr.params, maxParams),
			})
		}

		if fr.maxNesting > maxNestingDepth {
			issues = append(issues, conventionIssue{
				Path:    path,
				Line:    fr.start + 1,
				Kind:    "deep-nesting",
				Sev:     sevInfo,
				Message: fmt.Sprintf("function '%s' has %d levels of nesting (threshold %d) — use early returns or extract conditionals", fr.name, fr.maxNesting, maxNestingDepth),
			})
		}
	}

	return issues
}

// funcRange describes a detected function's span and metadata.
type funcRange struct {
	name       string
	start      int // 0-indexed line
	end        int // 0-indexed line
	params     int
	maxNesting int
}

// detectFunctions returns approximate function boundaries for the given file.
// Uses language-specific function-start detection and brace-counting for end.
func detectFunctions(path string, lines []string) []funcRange {
	ext := strings.ToLower(filepath.Ext(path))
	var startRe *regexp.Regexp
	switch ext {
	case ".go":
		startRe = regexp.MustCompile(`^func\s+(?:\([^)]+\)\s+)?([A-Za-z_]\w*)\s*\(`)
	case ".js", ".jsx", ".ts", ".tsx":
		startRe = regexp.MustCompile(`(?m)^(?:export\s+(?:default\s+)?)?(?:async\s+)?(?:function\s+([A-Za-z_$][\w$]*)|const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?\()`)
	case ".py":
		startRe = regexp.MustCompile(`^(?:async\s+)?def\s+([a-zA-Z_]\w*)\s*\(`)
	case ".rs":
		startRe = regexp.MustCompile(`(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][\w]*)\s*\(`)
	case ".java":
		startRe = regexp.MustCompile(`\s*(?:public|protected|private)?\s*(?:static\s+)?(?:[A-Za-z_][\w<>\[\]]*\s+)+([A-Za-z_][\w]*)\s*\(`)
	case ".cs":
		startRe = regexp.MustCompile(`\s*(?:public|protected|private|internal)?\s*(?:static\s+)?(?:async\s+)?(?:[A-Za-z_][\w<>\[\]]*\s+)+([A-Za-z_][\w]*)\s*\(`)
	case ".php":
		startRe = regexp.MustCompile(`function\s+([A-Za-z_]\w*)\s*\(`)
	case ".rb":
		startRe = regexp.MustCompile(`(?:def\s+([\w.]+)|->\s*\()`)
	case ".swift":
		startRe = regexp.MustCompile(`(?:func\s+([A-Za-z_]\w*)|init\s*\()`)
	case ".kt":
		startRe = regexp.MustCompile(`(?:fun\s+([A-Za-z_]\w*))\s*\(`)
	case ".dart":
		startRe = regexp.MustCompile(`(?:[\w<>]+\s+)?([A-Za-z_]\w*)\s*\(`)
	default:
		return nil
	}
	if startRe == nil {
		return nil
	}

	var ranges []funcRange
	for i, line := range lines {
		if m := startRe.FindStringSubmatch(line); m != nil {
			name := "(anonymous)"
			for _, g := range m[1:] {
				if g != "" {
					name = g
					break
				}
			}
			// Count parameters: text between first '(' and matching ')'
			params := countParams(line)
			// Find function end via brace counting
			end, nesting := findFuncEnd(lines, i)
			ranges = append(ranges, funcRange{name: name, start: i, end: end, params: params, maxNesting: nesting})
		}
	}
	return ranges
}

// countParams counts comma-separated parameters in a function signature line.
func countParams(line string) int {
	start := strings.Index(line, "(")
	if start < 0 {
		return 0
	}
	// Find matching close paren
	depth := 0
	for i := start; i < len(line); i++ {
		switch line[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				inner := line[start+1 : i]
				if strings.TrimSpace(inner) == "" {
					return 0
				}
				return strings.Count(inner, ",") + 1
			}
		}
	}
	return 0
}

// findFuncEnd finds the closing brace of a function starting at startLine.
// Returns the end line index and maximum nesting depth encountered.
func findFuncEnd(lines []string, startLine int) (int, int) {
	depth := 0
	maxNesting := 0
	started := false
	for i := startLine; i < len(lines); i++ {
		for _, ch := range lines[i] {
			switch ch {
			case '{':
				depth++
				started = true
				if depth > maxNesting {
					maxNesting = depth
				}
			case '}':
				depth--
				if started && depth == 0 {
					return i, maxNesting
				}
			}
		}
	}
	return len(lines) - 1, maxNesting
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
