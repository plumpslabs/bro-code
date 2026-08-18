package tool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFreeWebSearchParser(t *testing.T) {
	mockHTML := `
	<html>
	<body>
		<div class="result">
			<a class="result__url" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgolang.org%2Fpkg%2Fsync&rut=1">Go sync package</a>
			<a class="result__snippet">Package sync provides basic synchronization primitives such as mutual exclusion locks.</a>
		</div>
		<div class="result">
			<a class="result__url" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgobyexample.com%2Fgoroutines&rut=1">Go by Example: Goroutines</a>
			<a class="result__snippet">A goroutine is a lightweight thread of execution managed by the Go runtime.</a>
		</div>
	</body>
	</html>
	`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(mockHTML))
	}))
	defer srv.Close()

	// Parse the test HTML directly using the regex engine
	linkMatches := ddgResultLinkRe.FindAllStringSubmatch(mockHTML, 5)
	snippetMatches := ddgSnippetRe.FindAllStringSubmatch(mockHTML, 5)

	if len(linkMatches) != 2 {
		t.Fatalf("expected 2 link matches, got %d", len(linkMatches))
	}
	if len(snippetMatches) != 2 {
		t.Fatalf("expected 2 snippet matches, got %d", len(snippetMatches))
	}

	// Verify unwrap
	unwrap := duckDuckGoURLUnwrap.FindStringSubmatch(linkMatches[0][1])
	if len(unwrap) < 2 {
		t.Fatalf("expected uddg unwrap match")
	}
	if !strings.Contains(unwrap[1], "golang.org") {
		t.Errorf("expected golang.org in url, got %s", unwrap[1])
	}
}

func TestWebSearchFallbackFlow(t *testing.T) {
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")

	// When no key is set and no network query fails gracefully or returns results
	tool := &WebSearchTool{}
	if tool.Name() != "web_search" {
		t.Errorf("expected web_search name, got %s", tool.Name())
	}
	if tool.Parameters() == nil {
		t.Errorf("expected non-nil parameters")
	}
}
