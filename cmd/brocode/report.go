package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/report"
	"github.com/plumpslabs/bro-code/internal/store"
)

// runReportCommand implements `brocode report` — a local-first, privacy-safe
// way to turn the persisted event log into a usage/benchmark dataset. It never
// surfaces message text or file contents, only aggregate metrics + anomaly
// flags. Output can be Markdown or JSON, per-session or bulk.
//
//	brocode report <id>                 # single session, markdown
//	brocode report <id> --json          # single session, JSON
//	brocode report --all                # every session
//	brocode report --all --anomalies    # only flagged sessions
//	brocode report --all --since 2026-01-01
//	brocode report --summarize          # cross-session benchmark
//	brocode report --all --out bench.json --format json
func runReportCommand(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	jsonFmt := fs.Bool("json", false, "Emit JSON instead of Markdown")
	allF := fs.Bool("all", false, "Report every session (bulk export)")
	since := fs.String("since", "", "Only sessions created at/after this date (RFC3339 or YYYY-MM-DD); with --all")
	out := fs.String("out", "", "Write output to this file instead of stdout")
	anomaliesOnly := fs.Bool("anomalies", false, "Only sessions that have anomalies (with --all)")
	summarize := fs.Bool("summarize", false, "Cross-session benchmark summary")
	format := fs.String("format", "", "Override output format: md|json")
	_ = fs.Parse(reorderReportArgs(args))

	st, err := store.NewStore("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "brocode report:", err)
		os.Exit(1)
	}

	useJSON := *jsonFmt
	if *format == "json" {
		useJSON = true
	} else if *format == "md" {
		useJSON = false
	}

	if *summarize {
		reports, err := report.BuildAll(st, parseSince(*since))
		if err != nil {
			fmt.Fprintln(os.Stderr, "brocode report:", err)
			os.Exit(1)
		}
		agg := report.Summarize(reports)
		if useJSON {
			j, err := agg.RenderJSON()
			if err != nil {
				fmt.Fprintln(os.Stderr, "brocode report:", err)
				os.Exit(1)
			}
			writeOut(*out, j+"\n")
		} else {
			writeOut(*out, agg.RenderMarkdown())
		}
		return
	}

	if *allF {
		reports, err := report.BuildAll(st, parseSince(*since))
		if err != nil {
			fmt.Fprintln(os.Stderr, "brocode report:", err)
			os.Exit(1)
		}
		var sb strings.Builder
		for i, r := range reports {
			if *anomaliesOnly && len(r.Anomalies) == 0 {
				continue
			}
			if useJSON {
				j, _ := r.RenderJSON()
				sb.WriteString(j)
				sb.WriteString("\n")
			} else {
				sb.WriteString(r.RenderMarkdown())
				if i < len(reports)-1 {
					sb.WriteString("\n---\n\n")
				}
			}
		}
		writeOut(*out, sb.String())
		return
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "brocode report: missing session ID\nusage: brocode report <id> [--json] | --all | --summarize")
		os.Exit(2)
	}
	r, err := report.Build(st, rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "brocode report:", err)
		os.Exit(1)
	}
	if *anomaliesOnly && len(r.Anomalies) == 0 {
		writeOut(*out, "No anomalies detected for session "+r.SessionID+"\n")
		return
	}
	if useJSON {
		j, err := r.RenderJSON()
		if err != nil {
			fmt.Fprintln(os.Stderr, "brocode report:", err)
			os.Exit(1)
		}
		writeOut(*out, j+"\n")
	} else {
		writeOut(*out, r.RenderMarkdown())
	}
}

// parseSince parses a date filter, returning the zero time when empty/invalid
// (which BuildAll treats as "no filter").
func parseSince(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// writeOut prints to stdout, or to a file when out is set.
func writeOut(out, content string) {
	if out == "" {
		fmt.Print(content)
		return
	}
	if err := os.WriteFile(out, []byte(content), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "brocode report: write:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "wrote", out)
}

// reorderReportArgs separates flags and their values from positional arguments,
// allowing flags to appear after positional arguments (e.g. `report <id> --json`).
func reorderReportArgs(args []string) []string {
	var flagArgs, positional []string
	flagsWithValue := map[string]bool{
		"-since": true, "--since": true,
		"-out": true, "--out": true,
		"-format": true, "--format": true,
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if flagsWithValue[arg] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
		i++
	}
	return append(flagArgs, positional...)
}
