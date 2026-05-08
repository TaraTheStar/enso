// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
	"golang.org/x/net/html"
)

// ddgBackend scrapes html.duckduckgo.com/html/ — the only DDG endpoint
// that returns ranked organic results without an API key. Compared to
// SearXNG it returns no engine attribution and no publishedDate, but
// it works out of the box with no setup.
//
// Result anchor markup (stable for years):
//
//	<a class="result__a" href="//duckduckgo.com/l/?uddg=<urlenc-real-target>&...">title</a>
//	<a class="result__snippet" ...>snippet text</a>
//
// We walk the DOM rather than regex so a stray attribute or whitespace
// shift doesn't break extraction.
type ddgBackend struct {
	baseURL string // overridable for tests; defaults to html.duckduckgo.com
	client  *http.Client
}

func newDDGBackend(cfg config.SearchConfig) *ddgBackend {
	timeout := cfg.SearXNG.Timeout
	if timeout <= 0 {
		timeout = defaultSearchTimeout
	}
	return &ddgBackend{
		baseURL: "https://html.duckduckgo.com/html/",
		client:  &http.Client{Timeout: time.Duration(timeout) * time.Second},
	}
}

func (b *ddgBackend) Name() string { return "duckduckgo" }

func (b *ddgBackend) Search(ctx context.Context, query string, n int) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", query)
	form.Set("kl", "us-en")

	req, err := http.NewRequestWithContext(ctx, "POST", b.baseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	// DDG serves an empty results page when the UA looks botty. Use a
	// realistic desktop-Chrome string.
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	out := extractDDGResults(doc, n)
	return out, nil
}

func extractDDGResults(root *html.Node, n int) []SearchResult {
	var out []SearchResult

	var current *SearchResult
	flush := func() {
		if current != nil && current.URL != "" {
			out = append(out, *current)
		}
		current = nil
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if len(out) >= n {
			return
		}
		if node.Type == html.ElementNode && node.Data == "div" {
			if hasClass(node, "result") || hasClass(node, "web-result") {
				flush()
				current = &SearchResult{}
			}
		}
		if node.Type == html.ElementNode && node.Data == "a" && current != nil {
			class := attr(node, "class")
			switch {
			case strings.Contains(class, "result__a"):
				current.Title = strings.TrimSpace(textContent(node))
				current.URL = decodeDDGURL(attr(node, "href"))
			case strings.Contains(class, "result__snippet"):
				if current.Snippet == "" {
					current.Snippet = strings.TrimSpace(textContent(node))
				}
			}
		}
		if node.Type == html.ElementNode && (node.Data == "div" || node.Data == "td") && current != nil {
			class := attr(node, "class")
			if strings.Contains(class, "result__snippet") && current.Snippet == "" {
				current.Snippet = strings.TrimSpace(textContent(node))
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
			if len(out) >= n {
				return
			}
		}
	}
	walk(root)
	flush()
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// decodeDDGURL turns DDG's redirect wrapper into the real target.
// Inputs look like `//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2F&rut=...`
// (or with explicit https://). Returns the decoded `uddg` value when
// present, otherwise the input unchanged.
func decodeDDGURL(href string) string {
	if href == "" {
		return ""
	}
	raw := href
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return href
	}
	if uddg := u.Query().Get("uddg"); uddg != "" {
		return uddg
	}
	return href
}

func attr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, name string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == name {
			return true
		}
	}
	return false
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
