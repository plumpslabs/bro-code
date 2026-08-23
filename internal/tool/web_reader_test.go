package tool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractCleanMarkdownFromHTML(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Meta WhatsApp Business API Documentation</title>
    <script>console.log("ads tracking");</script>
    <style>body { font-family: sans-serif; }</style>
</head>
<body>
    <nav class="navbar-menu">
        <a href="/home">Home</a>
        <a href="/pricing">Pricing</a>
    </nav>
    
    <div class="cookie-banner">
        Please accept cookies to continue.
    </div>

    <main class="main-content">
        <h1>Cloud API Overview</h1>
        <p>The <strong>WhatsApp Business Cloud API</strong> allows medium and large businesses to communicate with their customers at scale.</p>
        
        <h2>Key Endpoints</h2>
        <ul>
            <li><code>POST /v20.0/PHONE_NUMBER_ID/messages</code> - Send messages</li>
            <li><code>GET /v20.0/PHONE_NUMBER_ID</code> - Query account info</li>
        </ul>

        <blockquote>Official API is required for automated production messaging.</blockquote>

        <pre><code>curl -X POST \
  https://graph.facebook.com/v20.0/FROM_PHONE_NUMBER_ID/messages \
  -H 'Authorization: Bearer ACCESS_TOKEN'</code></pre>
    </main>

    <footer class="site-footer">
        <p>Copyright 2026 Meta Inc.</p>
    </footer>
</body>
</html>`

	md, err := ExtractCleanMarkdownFromHTML(html)
	if err != nil {
		t.Fatalf("ExtractCleanMarkdownFromHTML failed: %v", err)
	}

	// 1. Verify title and headings
	if !strings.Contains(md, "Meta WhatsApp Business API Documentation") {
		t.Fatalf("expected title in markdown, got:\n%s", md)
	}
	if !strings.Contains(md, "# Cloud API Overview") {
		t.Fatalf("expected h1 in markdown, got:\n%s", md)
	}

	// 2. Verify code blocks & lists
	if !strings.Contains(md, "```") || !strings.Contains(md, "graph.facebook.com") {
		t.Fatalf("expected code block in markdown, got:\n%s", md)
	}
	if !strings.Contains(md, "• `POST /v20.0/PHONE_NUMBER_ID/messages`") {
		t.Fatalf("expected list item in markdown, got:\n%s", md)
	}

	// 3. Verify boilerplate stripping (nav, scripts, cookies, footer stripped)
	if strings.Contains(md, "console.log") {
		t.Fatalf("script was not stripped from markdown")
	}
	if strings.Contains(md, "Please accept cookies") {
		t.Fatalf("cookie banner was not stripped from markdown")
	}
	if strings.Contains(md, "Copyright 2026 Meta Inc") {
		t.Fatalf("footer was not stripped from markdown")
	}
}

func TestFetchAndCleanURLServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintln(w, "<html><body><h1>Hello World</h1><script>evil()</script><p>Clean content</p></body></html>")
	}))
	defer ts.Close()

	ctx := context.Background()
	res, err := FetchAndCleanURL(ctx, ts.URL)
	if err != nil {
		t.Fatalf("FetchAndCleanURL failed: %v", err)
	}

	if !strings.Contains(res, "Hello World") || !strings.Contains(res, "Clean content") {
		t.Fatalf("expected clean content, got:\n%s", res)
	}
	if strings.Contains(res, "evil()") {
		t.Fatalf("script was not stripped")
	}
}
