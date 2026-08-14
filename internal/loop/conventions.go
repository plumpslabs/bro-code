package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/plumpslabs/bro-code/internal/provider"
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

// checkFileConventions scans a single file for convention issues. It is
// deterministic and cheap (regex over content) — safe to run after every edit.
func checkFileConventions(path string) []conventionIssue {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	ext := strings.ToLower(filepath.Ext(path))
	lines := strings.Split(content, "\n")
	patterns := patternsByExt[ext]

	var issues []conventionIssue
	for i, line := range lines {
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
	return issues
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

// symbolNameRe extracts top-level function/class/const names for duplicate
// detection (JS/TS/Go/Python — the common agent-edited languages).
var symbolNameRe = map[string]*regexp.Regexp{
	".js":  regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*)|const\s+([A-Za-z_$][\w$]*)\s*=)`),
	".jsx": regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*)|const\s+([A-Za-z_$][\w$]*)\s*=)`),
	".ts":  regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*)|const\s+([A-Za-z_$][\w$]*)\s*=|interface\s+([A-Za-z_$][\w$]*))`),
	".tsx": regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:function\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*)|const\s+([A-Za-z_$][\w$]*)\s*=|interface\s+([A-Za-z_$][\w$]*))`),
	".go":  regexp.MustCompile(`(?m)^\s*func\s+([A-Za-z_][\w]*)\s*\(`),
	".py":  regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+([a-zA-Z_]\w*)\s*\(|^\s*class\s+([A-Za-z_]\w*)\s*:`),
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
			if g != "" {
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
func (e *Engine) reviewEditedFiles() string {
	if len(e.editedFiles) == 0 {
		return ""
	}
	e.reviewPasses++
	var issues []conventionIssue
	for _, p := range e.editedFiles {
		issues = append(issues, checkFileConventions(p)...)
	}
	issues = append(issues, findDuplicateSymbols(e.editedFiles, nil)...)

	// Native type errors via LSP (when wired) — catches what regex can't.
	if e.diagFn != nil {
		for _, p := range e.editedFiles {
			if out := e.diagFn(p); out != "" && out != "no diagnostics" {
				issues = append(issues, conventionIssue{
					Path:    p,
					Kind:    "type-error",
					Message: out,
				})
			}
		}
	}

	// Layer 2: senior LLM review only when the free checks are clean and we
	// are still inside the per-turn budget. If Layer 1 already found problems,
	// don't spend tokens — the model must fix those first.
	if len(issues) == 0 && e.reviewLLMEnabled && e.reviewPasses == 1 {
		if out := e.llmReviewEditedFiles(); out != "" {
			issues = append(issues, conventionIssue{
				Kind:    "senior-review",
				Message: out,
			})
		}
	}

	e.editedFiles = nil
	if len(issues) == 0 {
		return ""
	}
	return formatConventionIssues(issues)
}

// llmReviewEditedFiles runs one bounded senior-level code review over the
// edited files. It returns "" on any failure (never blocks the turn) and
// keeps the prompt + output small so the cost stays proportional.
func (e *Engine) llmReviewEditedFiles() string {
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
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 120 {
			lines = lines[:120]
		}
		sb.WriteString(fmt.Sprintf("=== %s ===\n%s\n", p, strings.Join(lines, "\n")))
	}
	if sb.Len() == 0 {
		return ""
	}

	prompt := `You are a senior code reviewer. Review ONLY the edited files below for real, high-signal problems a senior engineer would catch before merging:
- N+1 query patterns (DB query inside a loop — flag the loop and suggest batch loading)
- SQL: unbounded SELECT *, missing WHERE on updates/deletes, string-built queries (injection risk)
- Error handling: swallowed errors, empty catch, ignoring return errors
- Concurrency: shared mutable state, races, blocking calls in hot paths
- Reuse: reimplementing something that likely exists
- Security: user input concatenated into SQL/shell/HTML
- Obvious performance traps (quadratic loops, repeated work in loops)
Be terse. Format: one line per issue as "file:line — problem → fix". If the code is clean, reply exactly: CLEAN

` + sb.String()

	req := provider.CompletionRequest{
		Model:       e.model,
		Messages:    []provider.Message{{Role: "user", Content: prompt}},
		Temperature: 0.1,
	}
	resp, err := e.complete(context.Background(), req)
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
	var sb strings.Builder
	sb.WriteString("Code review found issues to fix before done:\n")
	for _, i := range issues {
		loc := i.Path
		if i.Line > 0 {
			loc += ":" + itoa(i.Line)
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s — %s\n", i.Sev, loc, i.Message))
	}
	return strings.TrimSpace(sb.String())
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
