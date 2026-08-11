// Command brocode is the project skeleton entrypoint.
//
// Modes:
//   - no arguments        → TUI (Bubble Tea)
//   - --search <query>    → headless, print BM25 results (for CI/automation)
//   - --diff              → headless, print a sample Myers unified diff
//
// Headless and TUI share the same pipeline (Principle: terminal-native,
// headless-capable — not duplicated code paths).
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/plumpslabs/bro-code/internal/diff"
	"github.com/plumpslabs/bro-code/internal/search"
	"github.com/plumpslabs/bro-code/internal/tui"
)

// Build-time injected variables (see Makefile LDFLAGS).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Sample before/after for the Myers diff demo.
const (
	sampleBefore = `package main

func main() {
    fmt.Println("hello")
}
`
	sampleAfter = `package main

func main() {
    name := "brocode"
    fmt.Println("hello", name)
}
`
)

func main() {
	headlessSearch := flag.String("search", "", "headless: search tools/skills in the sample corpus")
	showDiff := flag.Bool("diff", false, "headless: print a sample Myers unified diff")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("brocode %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	// Shared pipeline: corpus → index → (TUI | headless).
	ix := search.New(search.SampleCorpus())
	sampleDiff := diff.Unified("main.go", "main.go", sampleBefore, sampleAfter)

	switch {
	case *headlessSearch != "":
		printSearch(ix, *headlessSearch)
	case *showDiff:
		fmt.Print(sampleDiff)
	default:
		runTUI(ix, sampleDiff)
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

func runTUI(ix *search.Index, sampleDiff string) {
	p := tea.NewProgram(tui.New(ix, sampleDiff), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
