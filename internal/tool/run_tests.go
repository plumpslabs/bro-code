package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// RunTestsTool runs the project's test/build command on demand with a
// structured pass/fail summary — the model can call it during TSR REPRODUCE
// (watch a failing test) and VERIFY (confirm the fix) phases, complementing the
// engine's automatic verification. The command plan is injected (the loop's
// richer language/monorepo detection) with a self-contained fallback so the
// tool works even un-wired.
type RunTestsTool struct {
	// Plan returns the shell command lines to run, in order. Nil falls back
	// to defaultTestPlan(), which detects common configs in the cwd.
	Plan func() []string
}

func (t *RunTestsTool) Name() string { return "run_tests" }

func (t *RunTestsTool) Description() string {
	return "Run this project's tests/typecheck now and return a structured pass/fail summary (per-command exit status, counts when parseable, and the failing lines). Use it to REPRODUCE a reported failure before fixing (confirm the test FAILS first) and to VERIFY a fix (confirm it PASSES). Falls back to auto-detected commands; returns the exact commands it ran."
}

func (t *RunTestsTool) Parameters() map[string]any {
	return map[string]any{}
}

// testResult is one command's run summary.
type testResult struct {
	cmd     string
	exit    int
	ok      bool
	summary string
	fails   []string
	raw     string
}

func (t *RunTestsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	cmds := t.testCommands()
	if len(cmds) == 0 {
		return "run_tests: no test/build commands detected for this project (no go.mod/Cargo.toml/package.json/pytest config in the current directory).", nil
	}

	results := make([]testResult, 0, len(cmds))
	for _, cmd := range cmds {
		tr := runTestCommand(ctx, cmd)
		results = append(results, tr)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("run_tests: executed %d command(s)\n", len(results)))
	failures := 0
	for _, tr := range results {
		status := "PASS"
		if !tr.ok {
			status = "FAIL"
			failures++
		}
		sb.WriteString(fmt.Sprintf("\n== [%s] %s ==\nexit: %d\nsummary: %s\n", status, tr.cmd, tr.exit, tr.summary))
		if len(tr.fails) > 0 {
			sb.WriteString("failures:\n")
			for _, f := range tr.fails {
				sb.WriteString("  - " + f + "\n")
			}
		} else if !tr.ok {
			sb.WriteString("output (first 300 chars):\n" + head(tr.raw, 300) + "\n")
		}
	}
	if failures == 0 {
		sb.WriteString("\nALL COMMANDS PASSED.\n")
	} else {
		sb.WriteString(fmt.Sprintf("\n%d command(s) FAILED — fix the failure above, then re-run run_tests to confirm.\n", failures))
	}
	return CapOutput(sb.String()), nil
}

func (t *RunTestsTool) testCommands() []string {
	if t.Plan != nil {
		if cmds := t.Plan(); len(cmds) > 0 {
			return cmds
		}
	}
	return defaultTestPlan()
}

// runTestCommand executes one shell command line with a bounded timeout and
// parses its output into a structured result.
func runTestCommand(ctx context.Context, cmd string) testResult {
	tr := testResult{cmd: cmd}
	tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var sh *exec.Cmd
	if runtime.GOOS == "windows" {
		if bashPath, err := exec.LookPath("bash"); err == nil {
			sh = SafeCommandContext(tctx, bashPath, "-c", cmd)
		} else {
			sh = SafeCommandContext(tctx, "cmd.exe", "/c", cmd)
		}
	} else {
		sh = SafeCommandContext(tctx, "sh", "-c", cmd)
	}
	out, err := sh.CombinedOutput()
	tr.raw = string(out)
	if tctx.Err() == context.DeadlineExceeded {
		tr.exit = -1
		tr.summary = "command timed out after 90s"
		return tr
	}
	if err != nil {
		tr.exit = exitCodeOf(err)
		tr.ok = false
	} else {
		tr.ok = true
	}
	tr.summary, tr.fails = summarizeTestOutput(tr.raw)
	return tr
}

var (
	goOkRe   = regexp.MustCompile(`(?m)^ok\s+\S+`)
	goFailRe = regexp.MustCompile(`(?m)^FAIL\s+\S+`)
	goTestRe = regexp.MustCompile(`(?m)^--- FAIL:\s+(.+)$`)
	pytestRe = regexp.MustCompile(`(?m)(\d+) passed, (\d+) failed(?:, (\d+) (?:error|skipped))?`)
	pytestEr = regexp.MustCompile(`(?m)FAILED\s+(.+)$`)
	jsTestRe = regexp.MustCompile(`(?m)^\s*✗\s+(.+)$`)
)

// summarizeTestOutput turns raw test output into a one-line summary plus the
// first failing test names (for structured context). Language-agnostic: Go,
// pytest, and generic FAIL/error markers.
func summarizeTestOutput(raw string) (summary string, fails []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "no output", nil
	}
	// Go: count passing/failing packages and pull failing test names.
	passed, failed := len(goOkRe.FindAllString(raw, -1)), len(goFailRe.FindAllString(raw, -1))
	if failed > 0 || (passed > 0 && strings.Contains(raw, "FAIL")) {
		for _, m := range goTestRe.FindAllStringSubmatch(raw, -1) {
			if len(fails) < 8 {
				fails = append(fails, m[1])
			}
		}
		return fmt.Sprintf("%d package(s) passed, %d package(s) failed", passed, failed), fails
	}
	if passed > 0 {
		return fmt.Sprintf("%d package(s) passed", passed), nil
	}
	// pytest: "N passed, M failed".
	if m := pytestRe.FindStringSubmatch(raw); len(m) >= 3 {
		summary = m[1] + " passed, " + m[2] + " failed"
		if len(m) > 3 && m[3] != "" {
			summary += ", " + m[3]
		}
		for _, fm := range pytestEr.FindAllStringSubmatch(raw, -1) {
			if len(fails) < 8 {
				fails = append(fails, fm[1])
			}
		}
		return summary, fails
	}
	// Generic: search for explicit failure lines.
	for _, m := range jsTestRe.FindAllStringSubmatch(raw, -1) {
		if len(fails) < 8 {
			fails = append(fails, m[1])
		}
	}
	// Count generic failure markers as a rough signal.
	failCount := strings.Count(raw, "FAIL") + strings.Count(raw, "Error:")
	if failCount > 0 && len(fails) == 0 {
		return fmt.Sprintf("%d failure marker(s) in output", failCount), nil
	}
	if strings.Contains(raw, "FAIL") {
		return "failed (markers present)", fails
	}
	return "passed (no failure markers)", nil
}

// exitCodeOf extracts a numeric exit code from an exec error (0 when unknown).
func exitCodeOf(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// defaultTestPlan detects common project configs in the cwd and returns the
// test commands to run — a self-contained fallback used when the tool is not
// wired with the loop's richer plan.
func defaultTestPlan() []string {
	switch {
	case fileExistsInCwd("go.mod"):
		return []string{"go test ./..."}
	case fileExistsInCwd("Cargo.toml"):
		return []string{"cargo test"}
	case fileExistsInCwd("package.json"):
		if s := scriptInPackageJSON("test"); s != "" {
			return []string{"npm run test"}
		}
		if s := scriptInPackageJSON("test:unit"); s != "" {
			return []string{"npm run test:unit"}
		}
		if s := scriptInPackageJSON("typecheck"); s != "" {
			return []string{"npm run typecheck"}
		}
	case fileExistsInCwd("pytest.ini"), fileExistsInCwd("conftest.py"):
		return []string{"python3 -m pytest -q"}
	case fileExistsInCwd("pyproject.toml"):
		return []string{"python3 -m compileall -q ."}
	}
	return nil
}

func fileExistsInCwd(name string) bool {
	st, err := os.Stat(name)
	return err == nil && !st.IsDir()
}

func scriptInPackageJSON(name string) string {
	data, err := os.ReadFile("package.json")
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Scripts[name])
}
