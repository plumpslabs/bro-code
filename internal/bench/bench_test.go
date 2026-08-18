package bench

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCasesValid guards the shipped benchmark suite without running it: every
// case must load from JSON, have a unique id and a non-empty prompt, and its
// setup + verify scripts must be syntactically valid shell (`sh -n`). Nothing
// is executed — no provider key, no Go toolchain — so CI can keep the suite
// honest while real runs stay a developer/CI-with-keys activity.
func TestCasesValid(t *testing.T) {
	cases, err := LoadCases(filepath.Join("..", "..", "bench", "cases.json"))
	if err != nil {
		t.Fatalf("load bench/cases.json: %v", err)
	}
	if len(cases) < 5 {
		t.Fatalf("expected a real suite, got %d cases", len(cases))
	}

	ids := map[string]bool{}
	for _, c := range cases {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Prompt) == "" {
			t.Errorf("case with empty id or prompt: %+v", c)
		}
		if ids[c.ID] {
			t.Errorf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = true
		if c.MaxIterations <= 0 {
			t.Errorf("case %q: maxIterations must be positive", c.ID)
		}
		for _, name := range []string{"setup", "verify"} {
			script := c.Setup
			if name == "verify" {
				script = c.Verify
			}
			if strings.TrimSpace(script) == "" {
				t.Errorf("case %q: %s script is empty", c.ID, name)
				continue
			}
			if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Errorf("case %q: %s script has a syntax error: %v\n%s", c.ID, name, err, out)
			}
		}
	}
}

// TestSummarizeAggregates verifies the report arithmetic (pass rate, means)
// and that per-case ordering is stable by ID.
func TestSummarizeAggregates(t *testing.T) {
	results := []Result{
		{ID: "b", Pass: true, Tokens: 100, Duration: 2_000_000_000},
		{ID: "a", Pass: false, Tokens: 300, Duration: 4_000_000_000},
		{ID: "c", Pass: true, Tokens: 200, Duration: 6_000_000_000},
	}
	rep := Summarize(results)
	if rep.Total != 3 || rep.Passed != 2 || rep.Failed != 1 {
		t.Errorf("counts wrong: %+v", rep)
	}
	if rep.PassRate != 66.66666666666666 {
		t.Errorf("pass rate = %v, want 66.67%%", rep.PassRate)
	}
	if rep.MeanTokens != 200 || rep.MeanDuration != 4_000_000_000 {
		t.Errorf("means wrong: %+v", rep)
	}
	if rep.PerCase[0].ID != "a" || rep.PerCase[2].ID != "c" {
		t.Errorf("per-case must be sorted by id: %v", ids(rep.PerCase))
	}
}

func ids(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
