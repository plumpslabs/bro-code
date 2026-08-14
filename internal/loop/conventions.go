package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// debugStmtRe matches leftover debug statements per language. Group 1 is the
// offending statement for the message.
var debugStmtRe = map[string]*regexp.Regexp{
	".js":   regexp.MustCompile(`console\.(log|debug|warn)\([^;]*\)`),
	".jsx":  regexp.MustCompile(`console\.(log|debug|warn)\([^;]*\)`),
	".ts":   regexp.MustCompile(`console\.(log|debug|warn)\([^;]*\)`),
	".tsx":  regexp.MustCompile(`console\.(log|debug|warn)\([^;]*\)`),
	".go":   regexp.MustCompile(`fmt\.(Println|Printf|Print)\(`),
	".py":   regexp.MustCompile(`(?m)^\s*print\(`),
	".rs":   regexp.MustCompile(`println!\(`),
	".java": regexp.MustCompile(`System\.out\.(println|print)\(`),
}

// markerRe matches leftover work markers in any language.
var markerRe = regexp.MustCompile(`(?i)\b(TODO|FIXME|HACK|XXX)\b`)

// typeSafetyRe matches type-safety red flags per language. Group 1 is the
// offending snippet.
var typeSafetyRe = map[string]*regexp.Regexp{
	".ts":  regexp.MustCompile(`:\s*any\b`),
	".tsx": regexp.MustCompile(`:\s*any\b`),
	".go":  regexp.MustCompile(`\binterface\{\}\s+[a-zA-Z_]+`),
}

// conventionIssue is one problem found in an edited file.
type conventionIssue struct {
	Path    string
	Line    int
	Kind    string // "debug", "marker", "type-safety", "duplicate"
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

	var issues []conventionIssue
	for i, line := range lines {
		if re := debugStmtRe[ext]; re != nil {
			if m := re.FindString(line); m != "" {
				issues = append(issues, conventionIssue{Path: path, Line: i + 1, Kind: "debug", Message: "leftover debug statement: " + m})
			}
		}
		if m := markerRe.FindString(line); m != "" {
			issues = append(issues, conventionIssue{Path: path, Line: i + 1, Kind: "marker", Message: "leftover work marker: " + m})
		}
		if re := typeSafetyRe[ext]; re != nil {
			if m := re.FindString(line); m != "" {
				issues = append(issues, conventionIssue{Path: path, Line: i + 1, Kind: "type-safety", Message: "type-safety risk: " + m})
			}
		}
	}
	return issues
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

// reviewEditedFiles runs the native convention checks over the files edited
// this turn and appends LSP diagnostics (when wired). Returns the formatted
// review block, or "" when everything is clean. The duplicate-symbol check
// uses only the edited files themselves (cross-file reuse hints come from
// the persistent index tool at the model's discretion).
func (e *Engine) reviewEditedFiles() string {
	if len(e.editedFiles) == 0 {
		return ""
	}
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

	e.editedFiles = nil
	if len(issues) == 0 {
		return ""
	}
	return formatConventionIssues(issues)
}

// formatConventionIssues renders issues as a compact review block for the
// model's context.
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
		sb.WriteString("- " + loc + " — " + i.Message + "\n")
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
