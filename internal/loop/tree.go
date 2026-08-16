package loop

import (
	"fmt"
	"strings"
	"sync"
)

// TurnSnapshot records state at a specific turn checkpoint.
type TurnSnapshot struct {
	TurnIndex int
	Prompt    string
	State     LoopState
	Files     []string
}

// TreeDebugger maintains the interactive time-travel snapshot tree.
type TreeDebugger struct {
	mu        sync.Mutex
	snapshots []TurnSnapshot
}

// NewTreeDebugger initializes a new Time-Travel Debugger store.
func NewTreeDebugger() *TreeDebugger {
	return &TreeDebugger{snapshots: []TurnSnapshot{}}
}

// RecordTurn records a turn state checkpoint.
func (td *TreeDebugger) RecordTurn(prompt string, state LoopState, editedFiles []string) {
	td.mu.Lock()
	defer td.mu.Unlock()

	idx := len(td.snapshots) + 1
	td.snapshots = append(td.snapshots, TurnSnapshot{
		TurnIndex: idx,
		Prompt:    prompt,
		State:     state,
		Files:     append([]string{}, editedFiles...),
	})
}

// TreeView renders the visual conversation state tree (for /tree command).
func (td *TreeDebugger) TreeView() string {
	td.mu.Lock()
	defer td.mu.Unlock()

	if len(td.snapshots) == 0 {
		return "⏳ Time-Travel Tree: No turn snapshots recorded yet."
	}

	var sb strings.Builder
	sb.WriteString("⏳ Time-Travel Debugger Snapshot Tree:\n")
	for _, s := range td.snapshots {
		fmt.Fprintf(&sb, "├── Turn #%d [%s]: %s (Edited: %d files)\n",
	s.TurnIndex, s.State, summarizePrompt(s.Prompt), len(s.Files))
	}
	return strings.TrimSpace(sb.String())
}

// UndoLast turns back to the previous snapshot.
func (td *TreeDebugger) UndoLast() (TurnSnapshot, bool) {
	td.mu.Lock()
	defer td.mu.Unlock()

	if len(td.snapshots) < 2 {
		return TurnSnapshot{}, false
	}

	td.snapshots = td.snapshots[:len(td.snapshots)-1]
	last := td.snapshots[len(td.snapshots)-1]
	return last, true
}

func summarizePrompt(p string) string {
	p = strings.TrimSpace(p)
	if len(p) > 40 {
		return p[:40] + "..."
	}
	return p
}
