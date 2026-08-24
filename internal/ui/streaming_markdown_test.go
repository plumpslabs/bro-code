package ui

import (
	"strings"
	"testing"
)

func TestCompleteStreamingMarkdown(t *testing.T) {
	// 1. Unclosed code block
	rawCode := "```go\nfunc main() {\n    fmt.Println(\"hello\")"
	completedCode := completeStreamingMarkdown(rawCode)
	if !strings.HasSuffix(completedCode, "```\n") {
		t.Fatalf("expected completed code block suffix, got: %q", completedCode)
	}

	// 2. Closed code block should not get double closing
	closedCode := "```go\nfunc main() {}\n```"
	if res := completeStreamingMarkdown(closedCode); res != closedCode {
		t.Fatalf("expected untouched closed code block, got: %q", res)
	}

	// 3. Unclosed inline backtick
	rawInline := "Use the `fetch_url"
	completedInline := completeStreamingMarkdown(rawInline)
	if !strings.HasSuffix(completedInline, "`") {
		t.Fatalf("expected trailing backtick, got: %q", completedInline)
	}

	// 4. Unclosed bold
	rawBold := "This is **important"
	completedBold := completeStreamingMarkdown(rawBold)
	if !strings.HasSuffix(completedBold, "**") {
		t.Fatalf("expected trailing bold stars, got: %q", completedBold)
	}

	// 5. Empty string
	if res := completeStreamingMarkdown(""); res != "" {
		t.Fatalf("expected empty string, got: %q", res)
	}
}

func TestRenderStreamingMarkdown(t *testing.T) {
	raw := "### Realtime Header\n\nThis is **bold** text and `code`."
	rendered := renderStreamingMarkdown(raw, 80)
	if !strings.Contains(rendered, "Realtime") || !strings.Contains(rendered, "bold") {
		t.Fatalf("expected header and bold text in rendered output, got: %q", rendered)
	}
	// Check that ANSI color codes or code block padding are present
	if !strings.Contains(rendered, "\x1b[") {
		t.Fatalf("expected ANSI styling in rendered output, got: %q", rendered)
	}
}

func TestGetFormattedStreamMemoization(t *testing.T) {
	m := &Model{}
	m.pendingStream = "### Streaming Chunk 1"

	// First call formats and caches
	res1 := m.getFormattedStream(80)
	if res1 == "" {
		t.Fatal("expected non-empty formatted stream")
	}
	if m.streamRenderCached == "" {
		t.Fatal("expected streamRenderCached to be set")
	}

	// Second call with same content returns cached instance instantly
	res2 := m.getFormattedStream(80)
	if res2 != res1 {
		t.Fatalf("expected memoized output to match, got %q vs %q", res1, res2)
	}
}
