package agentic

import (
	"fmt"
	"strings"
)

// OutputFilter defines a streamable filter for command outputs.
// It mirrors the RTK StreamFilter trait (FeedLine, Flush, OnExit).
type OutputFilter interface {
	// FeedLine processes a single line and returns the filtered output to emit (if any).
	FeedLine(line string) string
	// Flush is called at the end of the stream to emit any buffered output.
	Flush() string
	// OnExit is called after the command exits to emit any summary.
	OnExit(exitCode int) string
}

// FilterChain applies multiple filters sequentially.
type FilterChain struct {
	filters []OutputFilter
}

func NewFilterChain(filters ...OutputFilter) *FilterChain {
	return &FilterChain{filters: filters}
}

func (fc *FilterChain) ProcessLine(line string) string {
	current := line
	for _, f := range fc.filters {
		if current == "" {
			break
		}
		current = f.FeedLine(current)
	}
	return current
}

func (fc *FilterChain) Flush() string {
	var sb strings.Builder
	for _, f := range fc.filters {
		if out := f.Flush(); out != "" {
			sb.WriteString(out + "\n")
		}
	}
	return sb.String()
}

func (fc *FilterChain) OnExit(exitCode int) string {
	var sb strings.Builder
	for _, f := range fc.filters {
		if out := f.OnExit(exitCode); out != "" {
			sb.WriteString(out + "\n")
		}
	}
	return sb.String()
}

// DedupFilter collapses consecutive identical lines into a summary.
type DedupFilter struct {
	lastLine string
	count    int
}

func (f *DedupFilter) FeedLine(line string) string {
	if line == f.lastLine {
		f.count++
		return ""
	}
	out := ""
	if f.count > 1 {
		out = fmt.Sprintf("... [Repeated %d times]\n", f.count)
	}
	out += line
	f.lastLine = line
	f.count = 1
	return out
}

func (f *DedupFilter) Flush() string {
	if f.count > 1 {
		return fmt.Sprintf("... [Repeated %d times]", f.count)
	}
	return ""
}

func (f *DedupFilter) OnExit(exitCode int) string { return "" }

// TruncateFilter limits the total number of lines emitted.
type TruncateFilter struct {
	MaxLines   int
	linesEmitted int
	truncated    bool
}

func (f *TruncateFilter) FeedLine(line string) string {
	if f.linesEmitted >= f.MaxLines {
		if !f.truncated {
			f.truncated = true
			return fmt.Sprintf("... [Output truncated after %d lines]", f.MaxLines)
		}
		return ""
	}
	// Split by newline in case a previous filter joined lines
	lines := strings.Split(line, "\n")
	f.linesEmitted += len(lines)
	return line
}

func (f *TruncateFilter) Flush() string { return "" }

func (f *TruncateFilter) OnExit(exitCode int) string { return "" }
