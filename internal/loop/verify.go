package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/tool"
)

// checkCmd is one verification command (binary + args). dir overrides the
// working directory (used when the project config lives in a subdirectory,
// e.g. a monorepo package); empty means the process cwd.
type checkCmd struct {
	name string
	args []string
	dir  string
}

// planVerification detects the project type from its config files and returns
// the check commands to run, in order. It is language-agnostic (Go, JS/TS,
// Python, Rust, Java, Ruby, PHP, C#) and architecture-agnostic: configs are first looked for
// in the current directory, then one level down — so monorepos whose packages
// live under apps/*, packages/*, services/*, or backend/ frontend/ still get
// verified even when the root has no config of its own. An empty result means
// no recognized build/type system — the model simply isn't verified (no false
// failures).
func planVerification() []checkCmd {
	if cmds := planIn("."); len(cmds) > 0 {
		return cmds
	}
	// Monorepo fallback: scan subdirectories (up to 2 levels deep — covers
	// apps/*, packages/*, services/*, backend/ frontend/) for a project config
	// and run the checks there. Bounded: depth 2, max 60 dirs, heavy dirs
	// skipped, so this never becomes a full-tree walk.
	var candidates []string
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || isHeavyVerifyDir(e.Name()) {
			continue
		}
		candidates = append(candidates, e.Name())
		// Level 2: only for container-style dirs (packages, apps, services,
		// modules, components, libs, backend, frontend, src, server, client).
		if !isContainerDir(e.Name()) {
			continue
		}
		subs, err := os.ReadDir(e.Name())
		if err != nil {
			continue
		}
		for _, se := range subs {
			if se.IsDir() && !strings.HasPrefix(se.Name(), ".") && !isHeavyVerifyDir(se.Name()) && len(candidates) < 60 {
				candidates = append(candidates, filepath.Join(e.Name(), se.Name()))
			}
		}
	}
	for _, d := range candidates {
		if cmds := planIn(d); len(cmds) > 0 {
			for i := range cmds {
				cmds[i].dir = d
			}
			return cmds
		}
	}
	return nil
}

// isContainerDir reports whether a directory conventionally holds sub-projects
// (monorepo package containers), so depth-2 scanning stays focused.
func isContainerDir(name string) bool {
	switch name {
	case "apps", "app", "packages", "pkg", "services", "modules", "components",
		"libs", "lib", "backend", "frontend", "server", "client", "src", "cmd":
		return true
	}
	return false
}

// planIn plans verification for one specific directory.
func planIn(dir string) []checkCmd {
	switch {
	case fileExistsIn(dir, "go.mod"):
		return []checkCmd{
			{"go", []string{"build", "./..."}, dir},
			{"go", []string{"vet", "./..."}, dir},
			{"go", []string{"test", "./..."}, dir},
		}
	case fileExistsIn(dir, "Cargo.toml"):
		return []checkCmd{{"cargo", []string{"check", "--quiet"}, dir}}
	case fileExistsIn(dir, "package.json"):
		return planJSVerificationIn(dir)
	case anyExistsIn(dir, "pyproject.toml", "setup.py", "requirements.txt", "Pipfile"):
		var cmds []checkCmd
		cmds = append(cmds, checkCmd{"python3", []string{"-m", "compileall", "-q", "."}, dir})
		if anyExistsIn(dir, "pytest.ini", "conftest.py") || fileExistsIn(dir, "tests") {
			cmds = append(cmds, checkCmd{"pytest", []string{"-q"}, dir})
		}
		return cmds
	case fileExistsIn(dir, "pom.xml") && fileExistsIn(dir, "mvnw"):
		return []checkCmd{{"./mvnw", []string{"-q", "test"}, dir}}
	case (fileExistsIn(dir, "build.gradle") || fileExistsIn(dir, "build.gradle.kts")) && fileExistsIn(dir, "gradlew"):
		return []checkCmd{{"./gradlew", []string{"-q", "test"}, dir}}
	case fileExistsIn(dir, "Gemfile"):
		return planRubyVerificationIn(dir)
	case fileExistsIn(dir, "composer.json"):
		return planPHPVerificationIn(dir)
	case globExistsIn(dir, "*.sln"), globExistsIn(dir, "*.csproj"):
		return planDotNetVerificationIn(dir)
	}
	return nil
}

// planRubyVerificationIn plans Ruby checks: bundle exec rspec when a spec/
// directory exists, bundle exec rake test for a test/-only project. A Gemfile
// with neither plans nothing — there is no test suite to verify (no false
// failures).
func planRubyVerificationIn(dir string) []checkCmd {
	switch {
	case dirExistsIn(dir, "spec"):
		return []checkCmd{{"bundle", []string{"exec", "rspec"}, dir}}
	case dirExistsIn(dir, "test"):
		return []checkCmd{{"bundle", []string{"exec", "rake", "test"}, dir}}
	}
	return nil
}

// planPHPVerificationIn plans PHP checks only when the project looks installed
// (composer.lock or vendor/ present) — a bare composer.json means dependencies
// were never fetched, so composer may not even be on the machine and a check
// would be a false failure. Installed projects get composer validate plus
// vendor/bin/phpunit when it exists and phpunit.xml is configured.
func planPHPVerificationIn(dir string) []checkCmd {
	if !fileExistsIn(dir, "composer.lock") && !dirExistsIn(dir, "vendor") {
		return nil
	}
	cmds := []checkCmd{{"composer", []string{"validate", "--no-check-publish"}, dir}}
	if (fileExistsIn(dir, "phpunit.xml") || fileExistsIn(dir, "phpunit.xml.dist")) &&
		fileExistsIn(dir, "vendor/bin/phpunit") {
		cmds = append(cmds, checkCmd{"vendor/bin/phpunit", nil, dir})
	}
	return cmds
}

// planDotNetVerificationIn plans C# checks: dotnet build (solution or project),
// then dotnet test --no-build when a test project (*Tests.csproj/*Test.csproj)
// is present. Requires the .NET SDK, like Go/Rust require theirs.
func planDotNetVerificationIn(dir string) []checkCmd {
	cmds := []checkCmd{{"dotnet", []string{"build", "--nologo"}, dir}}
	if globExistsIn(dir, "*Tests.csproj") || globExistsIn(dir, "*Test.csproj") {
		cmds = append(cmds, checkCmd{"dotnet", []string{"test", "--no-build", "--nologo"}, dir})
	}
	return cmds
}

func fileExistsIn(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !st.IsDir()
}

// dirExistsIn reports whether name is a directory under dir.
func dirExistsIn(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && st.IsDir()
}

// globExistsIn reports whether any file matches the glob pattern under dir
// (used for .sln/.csproj and test-project detection, where exact names vary).
func globExistsIn(dir, pattern string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	return err == nil && len(matches) > 0
}

func anyExistsIn(dir string, names ...string) bool {
	for _, n := range names {
		if fileExistsIn(dir, n) {
			return true
		}
	}
	return false
}

// isHeavyVerifyDir skips directories that are never a project root.
func isHeavyVerifyDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "build", "out", "target", ".next",
		".nuxt", "coverage", ".cache", ".turbo", "__pycache__", ".venv", "venv",
		"Pods", ".gradle", "bin", "obj", ".git", "docs", "scripts", "test", "tests":
		return true
	}
	return false
}

// planJSVerificationIn plans JS/TS checks for a specific directory using that
// directory's own package manager and package.json. It prefers the "typecheck"
// script, falls back to `tsc --noEmit --skipLibCheck` when a tsconfig exists
// and tsc is locally installed, then runs the "lint" script when defined.
// Tests are deliberately NOT auto-run (they are often integration-heavy, slow,
// or enter interactive watch mode that never terminates — causing session hangs).
func planJSVerificationIn(dir string) []checkCmd {
	pm := detectJSManagerIn(dir)
	var cmds []checkCmd

	if scriptExistsIn(dir, "typecheck") {
		cmds = append(cmds, checkCmd{pm, []string{"run", "typecheck"}, dir})
	} else if fileExistsIn(dir, "tsconfig.json") && tscAvailableIn(dir, pm) {
		switch pm {
		case "bun":
			cmds = append(cmds, checkCmd{"bunx", []string{"tsc", "--noEmit", "--skipLibCheck"}, dir})
		case "pnpm":
			cmds = append(cmds, checkCmd{"pnpm", []string{"exec", "tsc", "--noEmit", "--skipLibCheck"}, dir})
		case "yarn":
			cmds = append(cmds, checkCmd{"yarn", []string{"tsc", "--noEmit", "--skipLibCheck"}, dir})
		default:
			cmds = append(cmds, checkCmd{"npx", []string{"--no-install", "tsc", "--noEmit", "--skipLibCheck"}, dir})
		}
	}

	if scriptExistsIn(dir, "lint") {
		cmds = append(cmds, checkCmd{pm, []string{"run", "lint"}, dir})
	}
	// NOTE: Test suites are intentionally NOT auto-run here. Jest and Vitest
	// enter interactive watch mode when there is no CI=true env var, causing
	// the verification step to hang indefinitely. The model can still run tests
	// manually via the bash tool when explicitly needed.
	return cmds
}

// detectJSManagerIn picks the package manager from the lockfiles present in dir.
func detectJSManagerIn(dir string) string {
	switch {
	case fileExistsIn(dir, "bun.lock"), fileExistsIn(dir, "bun.lockb"):
		return "bun"
	case fileExistsIn(dir, "pnpm-lock.yaml"):
		return "pnpm"
	case fileExistsIn(dir, "yarn.lock"):
		return "yarn"
	default:
		return "npm"
	}
}

// scriptExistsIn reports whether dir/package.json declares the given non-empty
// script (e.g. "typecheck" or "lint").
func scriptExistsIn(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts[name]) != ""
}

// tscAvailableIn reports whether a local TypeScript compiler exists in dir so
// the tsc --noEmit fallback runs without npx/bunx trying to install anything.
func tscAvailableIn(dir, pm string) bool {
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "tsc")); err == nil {
		return true
	}
	// bun/pnpm sometimes resolve tsc without node_modules/.bin — accept the
	// runner; a missing compiler will fail fast and be reported to the model.
	return pm == "bun" || pm == "pnpm"
}

// findProjectRootForFile walks up the directory hierarchy starting from the file's
// directory to find the nearest ancestor directory containing a recognized project config.
func findProjectRootForFile(filePath string) string {
	clean := filepath.ToSlash(filepath.Clean(filePath))
	dir := filepath.Dir(clean)
	for dir != "." && dir != "/" && dir != "" {
		base := filepath.Base(dir)
		if !isHeavyVerifyDir(base) && len(planIn(dir)) > 0 {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if len(planIn(".")) > 0 {
		return "."
	}
	return ""
}

// planVerificationForFiles plans verification checks scoped to the nearest project
// roots and specific packages containing editedFiles when possible, falling back to full repo plan.
func planVerificationForFiles(editedFiles []string) []checkCmd {
	if len(editedFiles) > 0 {
		subdirs := make(map[string]bool)
		for _, f := range editedFiles {
			if root := findProjectRootForFile(f); root != "" {
				subdirs[root] = true
			}
		}
		var subCmds []checkCmd
		for dir := range subdirs {
			if fileExistsIn(dir, "go.mod") {
				// Targeted Go package testing: if all edited files live in specific subpackages,
				// run build, vet, and test on those specific package paths for 10x faster feedback.
				pkgPaths := make(map[string]bool)
				for _, f := range editedFiles {
					rel, err := filepath.Rel(dir, f)
					if err == nil && !strings.HasPrefix(rel, "..") {
						pkgDir := filepath.Dir(rel)
						if pkgDir == "." {
							pkgPaths["."] = true
						} else {
							pkgPaths["./"+pkgDir] = true
						}
					}
				}
				if len(pkgPaths) > 0 && len(pkgPaths) <= 3 {
					for p := range pkgPaths {
						subCmds = append(subCmds,
							checkCmd{"go", []string{"build", p}, dir},
							checkCmd{"go", []string{"vet", p}, dir},
							checkCmd{"go", []string{"test", p}, dir},
						)
					}
					continue
				}
			}

			if cmds := planIn(dir); len(cmds) > 0 {
				for i := range cmds {
					cmds[i].dir = dir
				}
				subCmds = append(subCmds, cmds...)
			}
		}
		if len(subCmds) > 0 {
			return subCmds
		}
	}
	return planVerification()
}

// runVerification executes the planned checks in order and returns the first
// failing output (capped), or "" when everything passes.
// If editedFiles is non-empty, static analysis errors are filtered to ensure
// that compiler/linter failures in untouched, unrelated files (pre-existing baseline errors)
// do not falsely fail the turn and trap the model in a futile repair loop.
func runVerification(ctx context.Context, editedFiles ...string) string {
	for _, c := range planVerificationForFiles(editedFiles) {
		if out := runCheck(ctx, c); out != "" {
			if len(editedFiles) > 0 && isStaticAnalysisCheck(c) {
				if filtered := filterRelevantErrors(out, editedFiles); filtered != "" {
					return filtered
				}
				// All reported errors are in untouched files outside the edited set;
				// ignore pre-existing baseline failures.
				continue
			}
			return out
		}
	}
	return ""
}

func isStaticAnalysisCheck(c checkCmd) bool {
	switch c.name {
	case "tsc", "bunx", "pnpm", "yarn", "npm":
		for _, a := range c.args {
			if a == "tsc" || a == "typecheck" || a == "lint" || a == "--noEmit" {
				return true
			}
		}
	case "go":
		for _, a := range c.args {
			if a == "vet" {
				return true
			}
		}
	case "cargo":
		for _, a := range c.args {
			if a == "check" {
				return true
			}
		}
	case "python3", "python":
		for _, a := range c.args {
			if a == "compileall" {
				return true
			}
		}
	}
	return false
}

// filterRelevantErrors filters multi-file compiler/linter error output to keep only
// error blocks/lines that pertain to the files edited in this turn.
func filterRelevantErrors(out string, editedFiles []string) string {
	if len(editedFiles) == 0 || strings.TrimSpace(out) == "" {
		return out
	}

	targets := make([]string, 0, len(editedFiles)*2)
	for _, f := range editedFiles {
		clean := filepath.ToSlash(filepath.Clean(f))
		targets = append(targets, clean)
		base := filepath.Base(clean)
		if base != "." && base != "/" && len(base) > 2 {
			targets = append(targets, base)
		}
	}

	lines := strings.Split(out, "\n")
	var relevant []string
	for _, line := range lines {
		for _, t := range targets {
			if strings.Contains(line, t) {
				relevant = append(relevant, line)
				break
			}
		}
	}

	if len(relevant) == 0 {
		return ""
	}
	return strings.Join(relevant, "\n")
}

// runCheck runs one check command with a bounded timeout and caps its output
// so a wall of compiler errors can never flood the context window.
func runCheck(ctx context.Context, c checkCmd) string {
	checkCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := tool.SafeCommandContext(checkCtx, c.name, c.args...)
	if c.dir != "" {
		cmd.Dir = c.dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		return bcontext.TruncateToolOutput(string(out), 4000)
	}
	return ""
}

// TestCommandPlan returns the planned verification commands as shell command
// lines (for the model-callable run_tests tool), in order, or nil when the
// project has no recognized test/build system. Reuses planVerification so the
// harness gate and the on-demand tool never diverge.
func TestCommandPlan() []string {
	var lines []string
	for _, c := range planVerification() {
		line := c.name + " " + strings.Join(c.args, " ")
		if c.dir != "" && c.dir != "." {
			line = "cd " + c.dir + " && " + line
		}
		lines = append(lines, line)
	}
	return lines
}

// describeVerification returns a short human-readable summary of the planned
// checks (for the UI progress line), or "" when nothing will run.
func describeVerification() string {
	var parts []string
	for _, c := range planVerification() {
		label := c.name + " " + strings.Join(c.args, " ")
		if c.dir != "" {
			label += " (in " + c.dir + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " · ")
}

var errorLineRegex = regexp.MustCompile(`(?m)(?:^|[^\w/\\])([\w./\\-]+\.[a-zA-Z0-9]+)[:(](\d+)(?:[:,\s)]|$)`)

// AttachErrorSourceSnippets extracts file and line references from compiler/linter error output,
// reads surrounding code lines from disk, and appends a line-numbered source snippet so the model
// sees the exact syntax error context without guessing.
func AttachErrorSourceSnippets(repoRoot string, vetErr string, editedFiles []string) string {
	if strings.TrimSpace(vetErr) == "" {
		return vetErr
	}
	matches := errorLineRegex.FindAllStringSubmatch(vetErr, 5)
	if len(matches) == 0 {
		return vetErr
	}

	seen := make(map[string]bool)
	var snippets strings.Builder

	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		filePath := m[1]
		lineNum, err := strconv.Atoi(m[2])
		if err != nil || lineNum <= 0 {
			continue
		}
		fullPath := filePath
		if repoRoot != "" && !filepath.IsAbs(filePath) {
			fullPath = filepath.Join(repoRoot, filePath)
		}
		key := fmt.Sprintf("%s:%d", fullPath, lineNum)
		if seen[key] {
			continue
		}
		seen[key] = true

		data, rerr := os.ReadFile(fullPath)
		if rerr != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		totalLines := len(lines)
		if totalLines == 0 {
			continue
		}

		start := lineNum - 8
		if start < 1 {
			start = 1
		}
		end := lineNum + 8
		if end > totalLines {
			end = totalLines
		}

		snippets.WriteString(fmt.Sprintf("\n\n--- Source Context for %s (lines %d-%d) ---\n", filePath, start, end))
		for i := start; i <= end; i++ {
			prefix := "  "
			if i == lineNum {
				prefix = "> " // Highlight failing line
			}
			snippets.WriteString(fmt.Sprintf("%s%4d: %s\n", prefix, i, lines[i-1]))
		}
	}

	if snippets.Len() > 0 {
		return vetErr + "\n\n📍 CODE CONTEXT AROUND ERROR:" + snippets.String()
	}
	return vetErr
}




