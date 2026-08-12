package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/plumpslabs/bro-code/internal/search"
)

// BenchmarkUpdateCycle measures the performance of a single Update() call
// with a key press event. This simulates the hot path during user interaction.
func BenchmarkUpdateCycle(b *testing.B) {
	m := New(search.New(search.SampleCorpus()), "bench", "", false)
	m.width = 120
	m.height = 40
	m.started = true
	m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "benchmark test message"})
	m.refreshChat()

	msg := tea.KeyPressMsg(tea.Key{Code: 'a', Text: "a"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Update(msg)
	}
}

// BenchmarkViewCycle measures the performance of a single View() call.
// This is the render path that runs every frame.
func BenchmarkViewCycle(b *testing.B) {
	m := New(search.New(search.SampleCorpus()), "bench", "", false)
	m.width = 120
	m.height = 40
	m.started = true
	// Add some chat messages for realistic rendering
	for i := 0; i < 10; i++ {
		m.chat = appendChat(m.chat, chatMsg{role: roleUser, text: "test message " + string(rune('a'+i))})
		m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: "response message " + string(rune('a'+i))})
	}
	m.refreshChat()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkStreamingTick measures the performance of streaming token delivery.
// This simulates the 30fps streaming tick during LLM response.
func BenchmarkStreamingTick(b *testing.B) {
	m := New(search.New(search.SampleCorpus()), "bench", "", false)
	m.width = 120
	m.height = 40
	m.started = true
	m.streaming = true
	m.streamBuf = "This is a long streaming response that will be revealed character by character at 30fps to simulate real LLM token delivery. "
	m.chat = appendChat(m.chat, chatMsg{role: roleAgent, text: ""})
	m.refreshChat()

	msg := streamTickMsg{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Update(msg)
		if !m.streaming {
			// Reset for next iteration
			m.streaming = true
			m.streamBuf = "This is a long streaming response that will be revealed character by character at 30fps to simulate real LLM token delivery. "
		}
	}
}

// BenchmarkRefreshChat measures the performance of viewport content rebuild.
func BenchmarkRefreshChat(b *testing.B) {
	m := New(search.New(search.SampleCorpus()), "bench", "", false)
	m.width = 120
	m.height = 40
	m.started = true
	// Add realistic chat content
	for i := 0; i < 20; i++ {
		m.chat = appendChat(m.chat, chatMsg{
			role:  roleUser,
			text:  "User message " + string(rune('a'+i%26)) + " with some longer text to simulate real usage patterns",
			trace: []string{"● Read file.go", "● Edit main.go"},
		})
		m.chat = appendChat(m.chat, chatMsg{
			role:      roleAgent,
			text:      "Agent response with detailed explanation and code blocks. This simulates a real response from the LLM provider.",
			summary:   "thinking trace",
			content:   "plan → search index → tokenize → BM25 rank → top-k → format reply",
			collapsed: true,
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.refreshChat()
	}
}
