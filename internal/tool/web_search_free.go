package tool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ddgResultLinkRe    = regexp.MustCompile(`(?s)<a[^>]+class="result__url"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe       = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	htmlTagStripRe     = regexp.MustCompile(`<[^>]+>`)
	duckDuckGoURLUnwrap = regexp.MustCompile(`uddg=([^&]+)`)
)

type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// FreeWebSearch queries DuckDuckGo's public HTML endpoint as a zero-config fallback.
// It requires NO API key and runs with pure standard library HTTP + regex parsing.
func FreeWebSearch(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 10 {
		maxResults = 10
	}

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	formData := url.Values{}
	formData.Set("q", query)
	formData.Set("b", "")
	formData.Set("kl", "wt-wt") // Worldwide

	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "https://html.duckduckgo.com/html/", strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := httpClientSearch.Do(req)
	if err != nil {
		return nil, fmt.Errorf("public search connection error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("public search endpoint returned HTTP %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}
	html := string(bodyBytes)

	// Extract result titles & links
	linkMatches := ddgResultLinkRe.FindAllStringSubmatch(html, maxResults*2)
	snippetMatches := ddgSnippetRe.FindAllStringSubmatch(html, maxResults*2)

	var results []WebSearchResult
	for i, m := range linkMatches {
		rawURL := m[1]
		// Unwrap DuckDuckGo tracking redirect if present
		if unwrap := duckDuckGoURLUnwrap.FindStringSubmatch(rawURL); len(unwrap) > 1 {
			if decoded, err := url.QueryUnescape(unwrap[1]); err == nil && decoded != "" {
				rawURL = decoded
			}
		}

		rawURL = strings.TrimSpace(rawURL)
		if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
			rawURL = "https://" + strings.TrimPrefix(rawURL, "//")
		}

		title := strings.TrimSpace(htmlTagStripRe.ReplaceAllString(m[2], ""))
		if title == "" {
			title = rawURL
		}

		snippet := ""
		if i < len(snippetMatches) && len(snippetMatches[i]) > 1 {
			snippet = strings.TrimSpace(htmlTagStripRe.ReplaceAllString(snippetMatches[i][1], ""))
			snippet = strings.ReplaceAll(snippet, "\n", " ")
		}

		results = append(results, WebSearchResult{
			Title:   title,
			URL:     rawURL,
			Snippet: snippet,
		})

		if len(results) >= maxResults {
			break
		}
	}

	return results, nil
}
