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

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	boilerClassIDRe = regexp.MustCompile(`(?i)(cookie|banner|advert|sidebar|social|share|breadcrumb|popup|modal|disclaimer|footer|header|menu|nav)`)
	multipleNewlineRe = regexp.MustCompile(`\n{3,}`)
	spaceCollapseRe   = regexp.MustCompile(`[ \t]+`)
)

// ExtractCleanMarkdownFromHTML parses raw HTML into a DOM tree, prunes boilerplate (scripts, ads, nav),
// and converts the main content into clean, token-efficient Markdown.
func ExtractCleanMarkdownFromHTML(rawHTML string) (string, error) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return "", err
	}

	// 1. Prune boilerplate DOM nodes
	pruneDOMNode(doc)

	// 2. Extract title if present
	title := extractTitle(doc)

	// 3. Render clean markdown from pruned DOM
	var sb strings.Builder
	if title != "" {
		sb.WriteString("# " + title + "\n\n")
	}

	renderNodeToMarkdown(doc, &sb)

	out := sb.String()
	out = multipleNewlineRe.ReplaceAllString(out, "\n\n")
	out = strings.TrimSpace(out)

	// Hard cap for LLM safety (max ~15KB text, ~3.5k tokens)
	if len(out) > 16000 {
		out = out[:16000] + "\n\n… [Content truncated for token efficiency]"
	}

	return out, nil
}

// pruneDOMNode recursively removes boilerplate, navigation, styling, and ads.
func pruneDOMNode(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if shouldPruneNode(c) {
			n.RemoveChild(c)
		} else {
			pruneDOMNode(c)
		}
	}
}

func shouldPruneNode(n *html.Node) bool {
	if n.Type == html.CommentNode {
		return true
	}
	if n.Type == html.ElementNode {
		switch n.DataAtom {
		case atom.Script, atom.Style, atom.Noscript, atom.Svg, atom.Canvas,
			atom.Nav, atom.Footer, atom.Header, atom.Aside, atom.Form,
			atom.Iframe, atom.Object, atom.Embed, atom.Dialog:
			return true
		}

		// Check classes and IDs for boilerplate markers
		for _, attr := range n.Attr {
			if (attr.Key == "class" || attr.Key == "id" || attr.Key == "role") && boilerClassIDRe.MatchString(attr.Val) {
				// Don't prune if it's the main container (e.g. "main-content")
				lower := strings.ToLower(attr.Val)
				if strings.Contains(lower, "main") || strings.Contains(lower, "article") || strings.Contains(lower, "post") || strings.Contains(lower, "doc") {
					continue
				}
				return true
			}
		}
	}
	return false
}

func extractTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.DataAtom == atom.Title {
		if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
			return strings.TrimSpace(n.FirstChild.Data)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := extractTitle(c); t != "" {
			return t
		}
	}
	return ""
}

func renderNodeToMarkdown(n *html.Node, sb *strings.Builder) {
	if n == nil {
		return
	}

	switch n.Type {
	case html.TextNode:
		text := spaceCollapseRe.ReplaceAllString(n.Data, " ")
		sb.WriteString(text)

	case html.ElementNode:
		switch n.DataAtom {
		case atom.H1:
			sb.WriteString("\n\n# ")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.H2:
			sb.WriteString("\n\n## ")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.H3:
			sb.WriteString("\n\n### ")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.H4, atom.H5, atom.H6:
			sb.WriteString("\n\n#### ")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.P:
			sb.WriteString("\n\n")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.Pre:
			sb.WriteString("\n\n```\n")
			renderRawText(n, sb)
			sb.WriteString("\n```\n\n")
		case atom.Code:
			if n.Parent != nil && n.Parent.DataAtom == atom.Pre {
				renderChildren(n, sb)
			} else {
				sb.WriteString("`")
				renderChildren(n, sb)
				sb.WriteString("`")
			}
		case atom.Blockquote:
			sb.WriteString("\n\n> ")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.Ul, atom.Ol:
			sb.WriteString("\n\n")
			renderChildren(n, sb)
			sb.WriteString("\n\n")
		case atom.Li:
			sb.WriteString("\n• ")
			renderChildren(n, sb)
		case atom.Strong, atom.B:
			sb.WriteString("**")
			renderChildren(n, sb)
			sb.WriteString("**")
		case atom.Em, atom.I:
			sb.WriteString("*")
			renderChildren(n, sb)
			sb.WriteString("*")
		case atom.A:
			href := ""
			for _, a := range n.Attr {
				if a.Key == "href" {
					href = a.Val
					break
				}
			}
			var linkText strings.Builder
			renderChildren(n, &linkText)
			cleanText := strings.TrimSpace(linkText.String())
			if cleanText != "" && href != "" && !strings.HasPrefix(href, "javascript:") {
				sb.WriteString(fmt.Sprintf("[%s](%s)", cleanText, href))
			} else if cleanText != "" {
				sb.WriteString(cleanText)
			}
		case atom.Tr:
			sb.WriteString("\n| ")
			renderChildren(n, sb)
		case atom.Th, atom.Td:
			renderChildren(n, sb)
			sb.WriteString(" | ")
		case atom.Br:
			sb.WriteString("\n")
		case atom.Hr:
			sb.WriteString("\n\n---\n\n")
		default:
			renderChildren(n, sb)
		}
	default:
		renderChildren(n, sb)
	}
}

func renderChildren(n *html.Node, sb *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNodeToMarkdown(c, sb)
	}
}

func renderRawText(n *html.Node, sb *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		} else {
			renderRawText(c, sb)
		}
	}
}

// FetchAndCleanURL fetches a remote webpage and returns clean, DOM-pruned Markdown.
func FetchAndCleanURL(ctx context.Context, targetURL string) (string, error) {
	parsed, err := url.Parse(targetURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("only http(s) URLs are supported: %s", targetURL)
	}

	tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := httpClientSearch.Do(req)
	if err != nil {
		lowerErr := strings.ToLower(err.Error())
		if strings.Contains(lowerErr, "x509") || strings.Contains(lowerErr, "certificate") {
			return "", fmt.Errorf("SSL_INTERCEPTED: SSL certificate invalid or intercepted by network for %s. Tip: Use Tavily search (set TAVILY_API_KEY) or check DNS", targetURL)
		}
		return "", fmt.Errorf("NETWORK_ERROR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("BLOCKED_HTTP_%d: Target website blocked automated access (Cloudflare / WAF)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP_ERROR_%d: Website returned status %s", resp.StatusCode, resp.Status)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return "", err
	}

	// Check if already text/plain or markdown
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/plain") || strings.Contains(contentType, "text/markdown") {
		text := string(bodyBytes)
		if len(text) > 16000 {
			text = text[:16000] + "\n\n… (truncated)"
		}
		return text, nil
	}

	return ExtractCleanMarkdownFromHTML(string(bodyBytes))
}
