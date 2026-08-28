package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ddgResultLinkRe     = regexp.MustCompile(`(?s)<a[^>]+(?:class="result__url"|class="result-link")[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe        = regexp.MustCompile(`(?s)<(?:a|td)[^>]+(?:class="result__snippet"|class="result-snippet")[^>]*>(.*?)</(?:a|td)>`)
	htmlTagStripRe      = regexp.MustCompile(`<[^>]+>`)
	duckDuckGoURLUnwrap = regexp.MustCompile(`uddg=([^&]+)`)
)

type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// FreeWebSearch queries public search endpoints with multi-tier fallback (DuckDuckGo HTML -> Lite -> Wikipedia).
// It requires NO API key and runs with pure standard library HTTP + regex parsing.
func FreeWebSearch(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 10 {
		maxResults = 10
	}

	// 1. Try DuckDuckGo HTML endpoint
	res, err := queryDDG(ctx, "https://html.duckduckgo.com/html/", query, maxResults)
	if err == nil && len(res) > 0 {
		return res, nil
	}

	// 2. Try DuckDuckGo Lite endpoint (often bypasses ISP blocks)
	res, err = queryDDG(ctx, "https://lite.duckduckgo.com/lite/", query, maxResults)
	if err == nil && len(res) > 0 {
		return res, nil
	}

	// 3. Fallback to Wikipedia OpenSearch for technical concepts/docs
	wikiRes, wikiErr := queryWikipedia(ctx, query, maxResults)
	if wikiErr == nil && len(wikiRes) > 0 {
		return wikiRes, nil
	}

	if err != nil {
		return nil, fmt.Errorf("public search connection error: %w", err)
	}
	return nil, nil
}

func queryDDG(ctx context.Context, endpoint, query string, maxResults int) ([]WebSearchResult, error) {
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	formData := url.Values{}
	formData.Set("q", query)
	formData.Set("b", "")
	formData.Set("kl", "wt-wt")

	req, err := http.NewRequestWithContext(tctx, http.MethodPost, endpoint, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := httpClientSearch.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}
	return parseDDGHTML(string(bodyBytes), maxResults), nil
}

func parseDDGHTML(html string, maxResults int) []WebSearchResult {
	linkMatches := ddgResultLinkRe.FindAllStringSubmatch(html, maxResults*3)
	snippetMatches := ddgSnippetRe.FindAllStringSubmatch(html, maxResults*3)

	var results []WebSearchResult
	for i, m := range linkMatches {
		rawURL := m[1]
		if unwrap := duckDuckGoURLUnwrap.FindStringSubmatch(rawURL); len(unwrap) > 1 {
			if decoded, err := url.QueryUnescape(unwrap[1]); err == nil && decoded != "" {
				rawURL = decoded
			}
		}

		rawURL = strings.TrimSpace(rawURL)
		if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
			rawURL = "https://" + strings.TrimPrefix(rawURL, "//")
		}

		// Validate URL to block IPv6 Zone IDs (SSRF/proxy bypass prevention)
		if parsed, perr := url.Parse(rawURL); perr == nil {
			if host := parsed.Hostname(); strings.Contains(host, "%") {
				continue // skip URLs with IPv6 Zone IDs
			}
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
	return results
}

func queryWikipedia(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	tctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	u := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=opensearch&search=%s&limit=%d&namespace=0&format=json", url.QueryEscape(query), maxResults)
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "BroCode-Assistant/1.0")

	resp, err := httpClientSearch.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wikipedia status %d", resp.StatusCode)
	}

	var data []any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data) < 4 {
		return nil, fmt.Errorf("invalid wikipedia response format")
	}

	titles, _ := data[1].([]any)
	snippets, _ := data[2].([]any)
	urls, _ := data[3].([]any)

	var results []WebSearchResult
	for i := 0; i < len(urls); i++ {
		tStr, _ := titles[i].(string)
		sStr, _ := snippets[i].(string)
		uStr, _ := urls[i].(string)
		if uStr != "" {
			results = append(results, WebSearchResult{
				Title:   tStr,
				URL:     uStr,
				Snippet: sStr,
			})
		}
	}
	return results, nil
}
