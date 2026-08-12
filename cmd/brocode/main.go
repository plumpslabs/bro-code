// Command brocode is the bro-code coding agent CLI.
//
// Modes:
//   - no arguments        → TUI (Bubble Tea v2 chat UI, landing screen)
//   - -c                  → TUI, resume the last saved session (~/.brocode/sessions)
//   - --search <query>    → headless, print BM25 results (for CI/automation)
//   - --diff              → headless, print a sample Myers unified diff
//   - --version           → print version and exit
//
// Headless and TUI share the same pipeline (Principle: terminal-native,
// headless-capable — not duplicated code paths).
package main

import (
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/plumpslabs/bro-code/internal/diff"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/tui"
	"github.com/plumpslabs/bro-code/internal/web"
)

// Build-time injected variables (see Makefile LDFLAGS).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	headlessSearch := flag.String("search", "", "headless: search tools/skills in the sample corpus")
	showDiff := flag.Bool("diff", false, "headless: print a sample Myers unified diff")
	showVersion := flag.Bool("version", false, "print version and exit")
	resume := flag.Bool("c", false, "resume the last session (no session file → fresh start)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("brocode %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if flag.Arg(0) == "web" {
		web.StartDashboard()
		return
	}

	// Shared pipeline: corpus → index → (TUI | headless).
	ix := search.New(search.SampleCorpus())

	switch {
	case *headlessSearch != "":
		printSearch(ix, *headlessSearch)
	case *showDiff:
		fmt.Print(diff.Sample())
	default:
		runTUI(ix, *resume)
	}
}

func printSearch(ix *search.Index, q string) {
	results := ix.Search(q, 5)
	if len(results) == 0 {
		fmt.Printf("query %q: no results\n", q)
		return
	}
	fmt.Printf("results for %q:\n", q)
	for _, r := range results {
		fmt.Printf("  %6.3f  %s (%s)\n", r.Score, r.Title, r.ID)
	}
}

func runTUI(ix *search.Index, resume bool) {
	p := tea.NewProgram(tui.New(ix, version, commit, resume))
	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	// Persist the conversation only if it actually started (Principle 5:
	// single latest.jsonl, bounded). Failure to save is a warning, not an
	// error — the user's session must never be held hostage by the disk.
	if m, ok := final.(tui.Model); ok && m.Started() {
		if err := tui.SaveSession(m.Messages()); err == nil {
			fmt.Printf("✓ Session saved for project '%s' (id: %s)\n", m.ProjectName(), m.SessionID())
			fmt.Printf("  File: %s\n", tui.SessionFilePath())
			fmt.Println("  Resume anytime in this project with: brocode -c")
		} else {
			fmt.Fprintln(os.Stderr, "warning: could not save session:", err)
		}
	}
}
