package agentic

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ToolOptions defines limits for tool execution.
type ToolOptions struct {
	Timeout time.Duration
}

// RunCommandNative runs a shell command with a timeout.
func RunCommandNative(cmdString string, opts ToolOptions) (string, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", cmdString)
	
	// Set up the generic output filters (Truncate to 1000 lines, Dedup)
	filters := NewFilterChain(
		&DedupFilter{},
		&TruncateFilter{MaxLines: 1000},
	)
	
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("command timed out after %v", timeout)
	}
	
	// Stream the captured output through the filters
	var filteredSb strings.Builder
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if processed := filters.ProcessLine(line); processed != "" {
			filteredSb.WriteString(processed + "\n")
		}
	}
	if flushed := filters.Flush(); flushed != "" {
		filteredSb.WriteString(flushed + "\n")
	}
	if exitMsg := filters.OnExit(cmd.ProcessState.ExitCode()); exitMsg != "" {
		filteredSb.WriteString(exitMsg + "\n")
	}
	
	return strings.TrimSpace(filteredSb.String()), err
}

// WebSearch is a native stub for web research capabilities
func WebSearch(query string) string {
	// In a full implementation, this would call a search API (e.g., Tavily, DuckDuckGo)
	// For now, it returns a stub indicating it's ready to be hooked up to an API key.
	return "WebSearch is natively orchestrated. Provide an API key in config to activate live search."
}
