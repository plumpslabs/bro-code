package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	bcontext "github.com/plumpslabs/bro-code/internal/context"
	"github.com/plumpslabs/bro-code/internal/memory"
	"github.com/plumpslabs/bro-code/internal/prompt"
	"github.com/plumpslabs/bro-code/internal/provider"
	"github.com/plumpslabs/bro-code/internal/store"
)

// SetCompactModel routes compaction summarization to a (cheaper) model. Empty
// uses the active synthesis model.
func (e *Engine) SetCompactModel(m string) {
	e.compactModel = m
}

// SetProjectContext injects a compact structural overview of the project into
// every turn's system prompt (see search.BuildProjectContext). Empty disables.
func (e *Engine) SetProjectContext(pc string) {
	e.projectCtx = pc
}

// SetSkillCatalog injects the installed skill catalog (name + description
// only; the model loads each SKILL.md itself). Empty disables the skills
// block. The catalog is relevance-filtered by the prompt builder when it
// exceeds the tuning threshold.
func (e *Engine) SetSkillCatalog(entries []prompt.SkillEntry) {
	e.skillsEntries = entries
}

// SetTuning replaces the runtime tuning surface (block/rule toggles, skill
// catalog budgets). Nil keeps the defaults.
func (e *Engine) SetTuning(t *prompt.Tuning) {
	if t != nil {
		e.tuning = t
	}
}

// SetDetectedStacks wires the repo's detected languages (with evidence files)
// so the prompt builder can render a STACK hint and bias the skill-catalog
// ranking toward the repo.
func (e *Engine) SetDetectedStacks(stacks []prompt.Stack) {
	e.stacks = stacks
}

// skillForRead returns the catalog name of the skill whose SKILL.md a read_file
// call targets, or "" when the path is not a known skill file. Matching is
// suffix-based so both relative (.brocode/skills/go-workflow/SKILL.md) and
// absolute read paths resolve to the same skill.
func (e *Engine) skillForRead(path string) string {
	if len(e.skillsEntries) == 0 {
		return ""
	}
	clean := filepath.ToSlash(strings.TrimSpace(path))
	if filepath.Base(clean) != "SKILL.md" {
		return ""
	}
	for _, s := range e.skillsEntries {
		if s.Path == "" {
			continue
		}
		rel := filepath.ToSlash(s.Path)
		if strings.HasSuffix(clean, rel) || strings.HasSuffix(clean, "/"+s.Name+"/SKILL.md") {
			return s.Name
		}
	}
	return ""
}

// SetRepoMap injects the deterministic project map (entry points, structure,
// hot files by usage) into every turn's system prompt. Empty disables.
func (e *Engine) SetRepoMap(rm string) {
	e.repoMap = rm
}

// SetScopeFiles injects the full project file list for smart scope
// pre-selection: given a user prompt, ScoreFiles() ranks files by relevance
// so BroCode can focus exploration on the most likely targets instead of
// scanning the entire workspace. Empty disables.
func (e *Engine) SetScopeFiles(files []string) {
	e.repoFiles = files
}

// SetMemoryStore wires the cross-session project memory. When set, a
// warm-start excerpt of past sessions' learnings is injected into the system
// prompt, and compaction summaries are auto-merged back into memory.
func (e *Engine) SetMemoryStore(st *memory.Store) {
	e.mem = st
	// Auto-prune stale memory facts (>30 days old) to keep the memory file
	// focused on the current project state. Cheap (single file read), runs once
	// per session. Non-blocking: errors are silently ignored.
	if st != nil {
		go st.PruneStale()
	}
}

// SetKnowledgeStore wires the Smart Context Graph backend. When set, the engine
// queries it at turn-start for relevance-ranked file hints and injects them
// as a "SMART CONTEXT" block in the system prompt — helping the agent avoid
// re-scanning files it has already analyzed in prior sessions.
func (e *Engine) SetKnowledgeStore(st *store.Store) {
	e.knowledge = st
	// Best-effort prune of stale knowledge entries on session start. Cheap
	// (single indexed DELETE with WHERE) and safe to run synchronously —
	// avoids a goroutine race window during turn-one system prompt build.
	if st != nil {
		_, _ = st.PruneKnowledge()
	}
}

// modelCompactionSummary asks the active model to write the structured 5-part
// compaction summary from the messages that are about to be dropped. Returns
// ok=false on any failure (network, bad JSON, empty) so the caller falls back
// to the deterministic boilerplate summary — compaction must never break a turn.
func (e *Engine) modelCompactionSummary(ctx context.Context) (bcontext.CompactionSummary, bool) {
	msgs := e.context.Messages()
	if len(msgs) == 0 {
		return bcontext.CompactionSummary{}, false
	}

	// Compact() keeps the last 4 messages; summarize exactly what gets dropped.
	keep := 4
	if len(msgs) <= keep {
		keep = 0
	}
	drop := msgs[:len(msgs)-keep]

	transcript := compactionTranscript(drop)
	if strings.TrimSpace(transcript) == "" {
		return bcontext.CompactionSummary{}, false
	}

	prompt := "You are summarizing an ongoing software-agent conversation for context compaction. " +
		"Below is the transcript that is about to be compacted away. Produce a concise structured summary " +
		"that captures everything a continuing agent still needs to know. " +
		"Respond with ONLY a JSON object (no markdown fences, no prose) using EXACTLY this schema:\n" +
		"{\"goal\": string, \"files_touched\": [string], \"decisions_made\": [string], " +
		"\"next_action\": string, \"constraints\": string, \"open_questions\": [string], " +
		"\"last_known_state\": string}\n\n" +
		"next_action = the single most useful next step for the continuing agent. " +
		"constraints = hard rules it must not violate (verified facts, things already tried that failed, " +
		"scope boundaries). Keep each field tight.\n\n" +
		"TRANSCRIPT TO COMPACT:\n" + transcript

	summCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := e.adapter.Complete(summCtx, provider.CompletionRequest{
		Model:       e.compactionModel(),
		Messages:    []provider.Message{{Role: "user", Content: prompt}},
		Temperature: 0.2,
	})
	if err != nil {
		return bcontext.CompactionSummary{}, false
	}
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return bcontext.CompactionSummary{}, false
	}

	summary, ok := parseCompactionJSON(resp.Content)
	if !ok {
		return bcontext.CompactionSummary{}, false
	}
	if strings.TrimSpace(summary.Goal) == "" && strings.TrimSpace(summary.LastKnownState) == "" {
		return bcontext.CompactionSummary{}, false
	}
	return summary, true
}

// compactionModel returns the model to use for compaction summarization: the
// cheaper routing model when configured, otherwise the main synthesis model.
func (e *Engine) compactionModel() string {
	if e.compactModel != "" {
		return e.compactModel
	}
	return e.model
}

// compactionTranscript renders the to-be-dropped messages as a bounded text
// transcript so a summarizing model sees real context without re-bloating the
// window we are trying to shrink.
func compactionTranscript(msgs []provider.Message) string {
	var sb strings.Builder
	const maxTranscriptChars = 60000
	for _, m := range msgs {
		role := m.Role
		if m.ToolCallID != "" {
			role = "tool_result"
		}
		var part strings.Builder
		part.WriteString(role)
		part.WriteString(": ")
		if m.Content != "" {
			part.WriteString(m.Content)
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&part, "\n  [tool_call %s(%s)]", tc.Name, tc.Arguments)
			}
		}
		if sb.Len()+len(part.String()) > maxTranscriptChars {
			sb.WriteString("\n...[transcript truncated for compaction]...")
			break
		}
		sb.WriteString(part.String())
		sb.WriteString("\n")
	}
	return sb.String()
}

// parseCompactionJSON extracts a CompactionSummary from a model reply, tolerating
// surrounding markdown fences and stray prose before/after the JSON object.
func parseCompactionJSON(raw string) (bcontext.CompactionSummary, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return bcontext.CompactionSummary{}, false
	}
	end := strings.LastIndexByte(raw, '}')
	if end < start {
		return bcontext.CompactionSummary{}, false
	}
	var summary bcontext.CompactionSummary
	if err := json.Unmarshal([]byte(raw[start:end+1]), &summary); err != nil {
		return bcontext.CompactionSummary{}, false
	}
	return summary, true
}
