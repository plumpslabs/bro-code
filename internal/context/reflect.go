package context

import (
	"strings"

	"github.com/plumpslabs/bro-code/internal/store"
)

// Reflect consolidates raw experience/hotfile notes into higher-level,
// retrieval-cheap notes (facts/gotchas) using deterministic heuristics — no
// extra LLM call. This is the "reflect" step of the retain→recall→reflect
// discipline and is what keeps BroCode an "efficient anomaly": it compounds
// past sessions' signal without a vector stack, embeddings server, or a
// summarization round-trip.
//
// It is cheap (two indexed reads + a few UPSERTs) and safe to call at
// compaction boundaries and session end. Returns the number of distilled
// notes created/refreshed.
func Reflect(st *store.Store) (int, error) {
	if st == nil {
		return 0, nil
	}
	created := 0

	// 1) Hot files → durable facts. A file touched repeatedly this session is
	//    central infrastructure; surface it as a fact so future sessions start
	//    knowing it matters (and can prioritize scoping/reading it).
	hots, err := st.NotesByKind(store.NoteHotfile, 50)
	if err == nil {
		for _, h := range hots {
			if h.Weight < 1.6 { // only promote genuinely-revisited files
				continue
			}
			subject := "fact:" + strings.TrimPrefix(h.Subject, "file:")
			if err := st.RecordNote(store.NoteFact, subject,
				"Frequently touched file ("+itoa(int(h.Weight))+" actions recorded) — central to this codebase.",
				"reflection:hotfile", []string{"hotfile", h.Subject}); err == nil {
				created++
			}
		}
	}

	// 2) Repeated failures → gotchas. Group experience notes by subject and
	//    tool; if a (subject, tool) pair failed twice or more, distill a
	//    gotcha so the agent enters next time already knowing the trap.
	type failKey struct{ subject, tool string }
	failCount := map[failKey]int{}
	var order []failKey
	exps, err := st.NotesByKind(store.NoteExperience, 200)
	if err == nil {
		for _, e := range exps {
			if !strings.Contains(e.Provenance, "outcome=error") {
				continue
			}
			tool := toolFromProvenance(e.Provenance)
			k := failKey{subject: e.Subject, tool: tool}
			if _, ok := failCount[k]; !ok {
				order = append(order, k)
			}
			failCount[k]++
		}
	}
	for _, k := range order {
		if failCount[k] < 2 {
			continue
		}
		subject := "gotcha:" + strings.TrimPrefix(k.subject, "file:")
		if err := st.RecordNote(store.NoteGotcha, subject,
			"Repeatedly failed via "+k.tool+" ("+itoa(failCount[k])+" recorded failures) — check before re-attempting.",
			"reflection:failure", []string{"gotcha", k.tool, k.subject}); err == nil {
			created++
		}
	}

	return created, nil
}

func toolFromProvenance(p string) string {
	// provenance looks like "tool=read_file outcome=error"
	if _, after, ok := strings.Cut(p, "tool="); ok {
		rest := after
		if before, _, ok := strings.Cut(rest, " "); ok {
			return before
		}
		return rest
	}
	return "unknown"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
