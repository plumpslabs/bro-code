package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/plumpslabs/bro-code/internal/provider"
)

var httpClientContext7 = &http.Client{
	Timeout: 20 * time.Second,
}

// Context7Client communicates directly with Context7 REST API (https://context7.com/api/v1).
// Native Go implementation eliminates Node.js/npx overhead and MCP protocol fragility.
type Context7Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewContext7Client creates a new Context7 API client.
func NewContext7Client(apiKey string) *Context7Client {
	if apiKey == "" {
		apiKey = os.Getenv("CONTEXT7_API_KEY")
		if apiKey == "" {
			apiKey = provider.GetActiveContext7Key()
		}
	}
	return &Context7Client{
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    "https://context7.com/api/v1",
		httpClient: httpClientContext7,
	}
}

// LibrarySearchResult holds a library match from Context7.
type LibrarySearchResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Stars       int    `json:"stars,omitempty"`
}

// ResolveLibrary finds the most relevant library ID for a given package/framework name.
func (c *Context7Client) ResolveLibrary(ctx context.Context, libraryName string) (string, error) {
	libraryName = strings.TrimSpace(libraryName)
	if libraryName == "" {
		return "", fmt.Errorf("library name cannot be empty")
	}

	endpoint := fmt.Sprintf("%s/search?query=%s", c.baseURL, url.QueryEscape(libraryName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("context7 search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("context7 invalid API key (HTTP 401)")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("context7 search returned HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", err
	}

	var results struct {
		Libraries []LibrarySearchResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &results); err != nil {
		// Fallback: try parsing array directly
		var arr []LibrarySearchResult
		if err2 := json.Unmarshal(raw, &arr); err2 == nil && len(arr) > 0 {
			return arr[0].ID, nil
		}
		return "", fmt.Errorf("failed to parse context7 search response: %w", err)
	}

	if len(results.Libraries) == 0 {
		return "", fmt.Errorf("no library found matching %q on Context7", libraryName)
	}

	return results.Libraries[0].ID, nil
}

// GetDocs retrieves targeted documentation from Context7 for a library and specific query.
func (c *Context7Client) GetDocs(ctx context.Context, libraryID, query string) (string, error) {
	libraryID = strings.TrimSpace(libraryID)
	query = strings.TrimSpace(query)
	if libraryID == "" {
		return "", fmt.Errorf("library ID cannot be empty")
	}

	// Ensure libraryID starts with slash if format requires
	docPath := libraryID
	if !strings.HasPrefix(docPath, "/") {
		docPath = "/" + docPath
	}

	endpoint := fmt.Sprintf("%s/docs%s", c.baseURL, docPath)
	if query != "" {
		endpoint += fmt.Sprintf("?query=%s", url.QueryEscape(query))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("context7 docs request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("context7 authentication required or invalid key (HTTP 401)")
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("documentation for library %q not found on Context7", libraryID)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("context7 docs returned HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}

	var docResp struct {
		Content  string `json:"content"`
		Markdown string `json:"markdown"`
		Snippets []struct {
			Title string `json:"title"`
			Text  string `json:"text"`
			Code  string `json:"code"`
		} `json:"snippets"`
	}

	if err := json.Unmarshal(raw, &docResp); err == nil {
		if docResp.Markdown != "" {
			return strings.TrimSpace(docResp.Markdown), nil
		}
		if docResp.Content != "" {
			return strings.TrimSpace(docResp.Content), nil
		}
		if len(docResp.Snippets) > 0 {
			var sb strings.Builder
			for i, snip := range docResp.Snippets {
				sb.WriteString(fmt.Sprintf("### %d. %s\n%s\n", i+1, snip.Title, snip.Text))
				if snip.Code != "" {
					sb.WriteString(fmt.Sprintf("```\n%s\n```\n", snip.Code))
				}
				sb.WriteString("\n")
			}
			return strings.TrimSpace(sb.String()), nil
		}
	}

	// Plain text / markdown fallback
	return strings.TrimSpace(string(raw)), nil
}

// FetchUnifiedDocs executes 3-tier docs resolution cascade:
// 1. Tier 1: Local / llms.txt (workspace or direct docs)
// 2. Tier 2: Native Context7 REST API
// 3. Tier 3: Web Search Cascade (Tavily/Exa/Free)
func FetchUnifiedDocs(ctx context.Context, library, query string) (string, string, error) {
	library = strings.TrimSpace(library)
	query = strings.TrimSpace(query)

	// 1. Tier 2: Try Context7 Native REST
	c7 := NewContext7Client("")
	if c7.apiKey != "" {
		libID, err := c7.ResolveLibrary(ctx, library)
		if err == nil && libID != "" {
			docs, dErr := c7.GetDocs(ctx, libID, query)
			if dErr == nil && len(strings.TrimSpace(docs)) > 0 {
				return docs, "Context7 (Official Verified Docs)", nil
			}
		}
	}

	// 2. Tier 3 Fallback: Web Search
	searchQuery := fmt.Sprintf("%s %s documentation", library, query)
	results, err := FreeWebSearch(ctx, searchQuery, 5)
	if err == nil && len(results) > 0 {
		var sb strings.Builder
		for i, r := range results {
			sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, r.Title, r.URL))
			if r.Snippet != "" {
				sb.WriteString("   " + r.Snippet + "\n")
			}
		}
		return strings.TrimSpace(sb.String()), "Web Search Fallback", nil
	}

	return "", "", fmt.Errorf("could not resolve documentation for %s %s: %w", library, query, err)
}
