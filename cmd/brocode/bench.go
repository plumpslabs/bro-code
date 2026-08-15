package main

import (
	"context"
	"fmt"
	"os"

	"github.com/plumpslabs/bro-code/internal/bench"
	"github.com/plumpslabs/bro-code/internal/provider"
)

// runBenchmark runs the benchmark harness headless over a JSON manifest of
// cases and renders the summary report. Returns the process exit code: 0 when
// every case passed, 1 otherwise. It uses the same resolved provider+model as
// a normal session so bench results reflect real usage.
//
// Manifest is either a single case object or an array:
//
//	[
//	  {
//	    "id": "fix-broken-import",
//	    "prompt": "The project doesn't build. Fix it.",
//	    "setup": "printf 'package main\\nimport \"x\"\\nfunc main(){}\\n' > main.go",
//	    "verify": "go build ./...",
//	    "maxIterations": 15
//	  }
//	]
func runBenchmark(manifest string, adapter provider.ProviderAdapter, model string) int {
	cases, err := bench.LoadCases(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		return 1
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "bench: no cases found in manifest")
		return 1
	}

	runner := &bench.Runner{Adapter: adapter, Model: model, Verbose: true}
	results := runner.Run(context.Background(), cases)
	rep := bench.Summarize(results)
	fmt.Print(bench.RenderReport(rep))

	for _, r := range results {
		if !r.Pass {
			return 1
		}
	}
	return 0
}
