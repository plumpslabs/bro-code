package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Streaming render benchmarks — the doctrine (PHILOSOPHY.md P2 / TECH_STACK
// §2) requires the streaming frame to stay under 16ms (~60fps) and forbids
// full redraws per tick. These benches pin the real cost:
//
//   - BenchmarkStreamingFrame*  = one full refreshChat + View() cycle while a
//     long reply grows char-by-char inside a full bounded chat history (the
//     exact hot path every streamTickMsg and SSE delta runs).
//
// Run: go test ./internal/tui/ -run '^$' -bench 'BenchmarkStreamingFrame' -benchtime 200x
func benchStreamingFrame(b *testing.B, msgs, replyLen int) {
	m := newTestModel()
	m.started = true
	m.width, m.height = 120, 40
	m.layout()

	// Bounded chat history (maxHistory) with realistic markdown-heavy text.
	for i := 0; i < msgs; i++ {
		role := roleUser
		text := fmt.Sprintf("**question %d** with `code` and _emphasis_ and list:", i)
		if i%2 == 1 {
			role = roleAgent
			text = "Long answer " + strings.Repeat("**bold** and `inline` and words long enough to be wrapped by lipgloss.Wrap inside the viewport. ", 8) + fmt.Sprintf(" #%d", i)
		}
		m.chat = append(m.chat, chatMsg{role: role, text: text})
	}

	// The streaming reply grows by streamChunk chars per tick.
	m.streaming = true
	m.streamBuf = strings.Repeat("x", replyLen)
	m.chat = append(m.chat, chatMsg{role: roleAgent, text: ""})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(m.streamBuf) > 0 {
			n := min(streamChunk, len(m.streamBuf))
			m.chat[len(m.chat)-1].text += m.streamBuf[:n]
			m.streamBuf = m.streamBuf[n:]
		}
		m.refreshChat()
		_ = m.View().Content
	}
}

// BenchmarkStreamingFrameSmall is a typical session: a few turns, short reply.
func BenchmarkStreamingFrameSmall(b *testing.B) { benchStreamingFrame(b, 6, 2000) }

// BenchmarkStreamingFrameFull is the worst case: a full bounded history with
// a long reply — the frame cost must NOT scale with history size.
func BenchmarkStreamingFrameFull(b *testing.B) { benchStreamingFrame(b, maxHistory, 10000) }

// measureStreamingFrame runs the exact streaming hot path (grow reply +
// refreshChat + View) for iters iterations and returns the total elapsed
// time. Shared by the benchmarks above and the CI guard below.
func measureStreamingFrame(msgs, replyLen, iters int) time.Duration {
	m := newTestModel()
	m.started = true
	m.width, m.height = 120, 40
	m.layout()

	for i := 0; i < msgs; i++ {
		role := roleUser
		text := fmt.Sprintf("**question %d** with `code` and _emphasis_ and list:", i)
		if i%2 == 1 {
			role = roleAgent
			text = "Long answer " + strings.Repeat("**bold** and `inline` and words long enough to be wrapped by lipgloss.Wrap inside the viewport. ", 8) + fmt.Sprintf(" #%d", i)
		}
		m.chat = append(m.chat, chatMsg{role: role, text: text})
	}
	m.streaming = true
	m.streamBuf = strings.Repeat("x", replyLen)
	m.chat = append(m.chat, chatMsg{role: roleAgent, text: ""})

	start := time.Now()
	for i := 0; i < iters; i++ {
		if len(m.streamBuf) > 0 {
			n := min(streamChunk, len(m.streamBuf))
			m.chat[len(m.chat)-1].text += m.streamBuf[:n]
			m.streamBuf = m.streamBuf[n:]
		}
		m.refreshChat()
		_ = m.View().Content
	}
	return time.Since(start)
}

// TestStreamingFrameScale is the CI bench guard (threshold 2.0, deliberately
// loose so it stays anti-flaky on shared runners). With the per-message render
// cache, streaming must re-render ONLY the last message — a full bounded
// history costs about the same as a 6-message session. A regression to
// full-redraw-per-tick shows up here as a hard failure.
func TestStreamingFrameScale(t *testing.T) {
	const iters = 200
	small := measureStreamingFrame(6, 2000, iters)
	full := measureStreamingFrame(maxHistory, 10000, iters)
	perSmall := float64(small) / iters
	perFull := float64(full) / iters
	ratio := perFull / perSmall
	t.Logf("streaming frame/op: small=%.1fµs full=%.1fµs ratio=%.2f (limit 2.0)", perSmall/1000, perFull/1000, ratio)
	if ratio > 2.0 {
		t.Fatalf("streaming frame scales with history: full %.1fµs > 2× small %.1fµs — render cache regression", perFull/1000, perSmall/1000)
	}
}

// renderChatMsg on one growing message — the ceiling of an incremental render
// where the stable prefix is cached and only the last message is re-rendered.
func BenchmarkRenderLastMessageOnly(b *testing.B) {
	m := newTestModel()
	m.started = true
	m.width, m.height = 120, 40
	m.layout()
	m.chat = append(m.chat, chatMsg{role: roleAgent, text: strings.Repeat("x", 10000)})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.chat[len(m.chat)-1].text += "y"
		_ = m.renderChatMsg(m.chat[len(m.chat)-1])
	}
}
