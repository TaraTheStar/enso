// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/TaraTheStar/enso/internal/config"
)

// SearchBackend is the swap-in implementation behind WebSearchTool.
// Backends are picked at startup by pickSearchBackend; the tool
// instance holds one and forwards the query.
type SearchBackend interface {
	Search(ctx context.Context, query string, n int) ([]SearchResult, error)
	Name() string
}

// SearchResult is the structured row each backend returns. PublishedDate
// is empty when the engine doesn't supply one (DDG never does).
type SearchResult struct {
	Title         string
	URL           string
	Snippet       string
	Engine        string
	PublishedDate string
}

const (
	defaultSearchMax     = 10
	defaultSearchTimeout = 15
	searchSnippetCap     = 240
	searchSummaryLines   = 100
)

// WebSearchTool dispatches search queries to a configured backend
// (SearXNG primary, DuckDuckGo fallback).
type WebSearchTool struct {
	backend    SearchBackend
	maxResults int
}

func (t WebSearchTool) Name() string { return "web_search" }

func (t WebSearchTool) Description() string {
	return fmt.Sprintf("Search the web and return ranked results (title, url, snippet). Backend: %s. Args: query (string, required), max_results (int, optional, capped at %d). Pair with web_fetch to read the pages it returns.", t.backend.Name(), t.maxResults)
}

func (t WebSearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query":       map[string]interface{}{"type": "string", "description": "Search query."},
			"max_results": map[string]interface{}{"type": "integer", "description": "Cap on results returned (1-N where N is the configured ceiling). Optional."},
		},
		"required": []string{"query"},
	}
}

func (t WebSearchTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	q, _ := args["query"].(string)
	q = strings.TrimSpace(q)
	if q == "" {
		return Result{}, fmt.Errorf("web_search: query required")
	}

	n := t.maxResults
	if v, ok := args["max_results"]; ok {
		switch x := v.(type) {
		case float64:
			if int(x) > 0 && int(x) < n {
				n = int(x)
			}
		case int:
			if x > 0 && x < n {
				n = x
			}
		}
	}

	results, err := t.backend.Search(ctx, q, n)
	if err != nil {
		return Result{LLMOutput: fmt.Sprintf("web_search: %v", err)}, nil
	}
	if len(results) == 0 {
		return Result{LLMOutput: fmt.Sprintf("web_search: no results for %q", q)}, nil
	}

	rendered := renderSearchResults(results)
	short, full := HeadTail(rendered, searchSummaryLines)
	return Result{LLMOutput: short, FullOutput: full}, nil
}

func renderSearchResults(results []SearchResult) string {
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, fallback(r.Title, "(untitled)"), r.URL)
		if s := strings.TrimSpace(r.Snippet); s != "" {
			if len(s) > searchSnippetCap {
				s = s[:searchSnippetCap] + "…"
			}
			fmt.Fprintf(&b, "   %s\n", s)
		}
		var meta []string
		if r.Engine != "" {
			meta = append(meta, r.Engine)
		}
		if r.PublishedDate != "" {
			meta = append(meta, r.PublishedDate)
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "   [%s]\n", strings.Join(meta, " · "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// RegisterSearch picks a backend from cfg and registers WebSearchTool.
// `provider = "none"` suppresses the tool entirely; any other value
// (including "" / unset) yields a working backend, with DuckDuckGo as
// the always-available fallback.
func RegisterSearch(r *Registry, cfg config.SearchConfig) {
	backend := pickSearchBackend(cfg)
	if backend == nil {
		return
	}
	max := cfg.SearXNG.MaxResults
	if max <= 0 {
		max = defaultSearchMax
	}
	r.Register(WebSearchTool{backend: backend, maxResults: max})
}

func pickSearchBackend(cfg config.SearchConfig) SearchBackend {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "none", "off", "disabled":
		return nil
	case "searxng":
		if cfg.SearXNG.Endpoint == "" {
			slog.Warn("search.provider=searxng but [search.searxng] endpoint empty; falling back to duckduckgo")
			return newDDGBackend(cfg)
		}
		return newSearXNGBackend(cfg)
	case "duckduckgo", "ddg":
		return newDDGBackend(cfg)
	case "":
		if cfg.SearXNG.Endpoint != "" {
			return newSearXNGBackend(cfg)
		}
		return newDDGBackend(cfg)
	default:
		slog.Warn("unknown search.provider; falling back to duckduckgo", "provider", cfg.Provider)
		return newDDGBackend(cfg)
	}
}
