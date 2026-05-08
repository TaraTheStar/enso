// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
)

func TestWebSearch_SearXNG_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "rust async" {
			t.Errorf("q = %q", got)
		}
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Errorf("format = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Async Rust", "url": "https://example.com/a", "content": "intro to async", "engine": "google", "publishedDate": "2024-05-01"},
				{"title": "Tokio guide", "url": "https://example.com/b", "content": "the runtime", "engine": "ddg"},
			},
		})
	}))
	defer srv.Close()

	cfg := config.SearchConfig{
		Provider: "searxng",
		SearXNG:  config.SearXNGConfig{Endpoint: srv.URL, MaxResults: 5},
	}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	tool := r.Get("web_search")
	if tool == nil {
		t.Fatal("web_search not registered")
	}

	res, err := tool.Run(context.Background(), map[string]any{"query": "rust async"}, newToolAC(t.TempDir()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"Async Rust", "https://example.com/a", "intro to async", "google · 2024-05-01", "Tokio guide"} {
		if !strings.Contains(res.LLMOutput, want) {
			t.Errorf("missing %q in:\n%s", want, res.LLMOutput)
		}
	}
}

func TestWebSearch_SearXNG_MaxResultsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results := make([]map[string]any, 8)
		for i := range results {
			results[i] = map[string]any{"title": "r", "url": "https://e/" + string(rune('a'+i)), "content": "x"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	cfg := config.SearchConfig{Provider: "searxng", SearXNG: config.SearXNGConfig{Endpoint: srv.URL, MaxResults: 3}}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	res, _ := r.Get("web_search").Run(context.Background(), map[string]any{"query": "x"}, newToolAC(t.TempDir()))
	// Three results numbered 1..3, no "4." line.
	if !strings.Contains(res.LLMOutput, "1.") || !strings.Contains(res.LLMOutput, "3.") {
		t.Errorf("missing 1./3. in %q", res.LLMOutput)
	}
	if strings.Contains(res.LLMOutput, "4.") {
		t.Errorf("got 4. when capped at 3:\n%s", res.LLMOutput)
	}
}

func TestWebSearch_SearXNG_AuthHeader(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	cfg := config.SearchConfig{Provider: "searxng", SearXNG: config.SearXNGConfig{Endpoint: srv.URL, APIKey: "secret-token"}}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	_, _ = r.Get("web_search").Run(context.Background(), map[string]any{"query": "x"}, newToolAC(t.TempDir()))
	if seen != "Bearer secret-token" {
		t.Errorf("auth = %q", seen)
	}
}

func TestWebSearch_SearXNG_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()

	cfg := config.SearchConfig{Provider: "searxng", SearXNG: config.SearXNGConfig{Endpoint: srv.URL}}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	res, err := r.Get("web_search").Run(context.Background(), map[string]any{"query": "x"}, newToolAC(t.TempDir()))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "HTTP 500") {
		t.Errorf("expected HTTP 500 in output, got %q", res.LLMOutput)
	}
}

func TestWebSearch_SearXNG_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	cfg := config.SearchConfig{Provider: "searxng", SearXNG: config.SearXNGConfig{Endpoint: srv.URL}}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	res, _ := r.Get("web_search").Run(context.Background(), map[string]any{"query": "x"}, newToolAC(t.TempDir()))
	if !strings.Contains(res.LLMOutput, "no results") {
		t.Errorf("expected 'no results', got %q", res.LLMOutput)
	}
}

func TestWebSearch_QueryRequired(t *testing.T) {
	r := NewRegistry()
	RegisterSearch(r, config.SearchConfig{Provider: "searxng", SearXNG: config.SearXNGConfig{Endpoint: "http://unused"}})
	_, err := r.Get("web_search").Run(context.Background(), map[string]any{}, newToolAC(t.TempDir()))
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestWebSearch_DDG_HappyPath(t *testing.T) {
	target := "https://example.com/foo?bar=baz"
	page := `
<html><body>
<div class="result">
  <h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=` + url.QueryEscape(target) + `&rut=xyz">Foo Site</a></h2>
  <a class="result__snippet" href="...">A snippet about foo with details.</a>
</div>
<div class="web-result">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=` + url.QueryEscape("https://other.example/") + `">Other</a>
  <div class="result__snippet">Short blurb.</div>
</div>
</body></html>
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	cfg := config.SearchConfig{Provider: "duckduckgo"}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	tool := r.Get("web_search").(WebSearchTool)
	tool.backend.(*ddgBackend).baseURL = srv.URL // redirect to test server

	res, err := tool.Run(context.Background(), map[string]any{"query": "foo"}, newToolAC(t.TempDir()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"Foo Site", target, "snippet about foo", "Other", "https://other.example/", "Short blurb"} {
		if !strings.Contains(res.LLMOutput, want) {
			t.Errorf("missing %q in:\n%s", want, res.LLMOutput)
		}
	}
	// Redirect wrapper must not leak through.
	if strings.Contains(res.LLMOutput, "duckduckgo.com/l/") {
		t.Errorf("DDG redirect leaked into output:\n%s", res.LLMOutput)
	}
}

func TestWebSearch_DDG_EmptyPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body></body></html>"))
	}))
	defer srv.Close()

	cfg := config.SearchConfig{Provider: "duckduckgo"}
	r := NewRegistry()
	RegisterSearch(r, cfg)
	tool := r.Get("web_search").(WebSearchTool)
	tool.backend.(*ddgBackend).baseURL = srv.URL

	res, _ := tool.Run(context.Background(), map[string]any{"query": "x"}, newToolAC(t.TempDir()))
	if !strings.Contains(res.LLMOutput, "no results") {
		t.Errorf("expected 'no results', got %q", res.LLMOutput)
	}
}

func TestPickSearchBackend(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.SearchConfig
		want string // backend name; "" = nil (suppressed)
	}{
		{"unset_no_endpoint", config.SearchConfig{}, "duckduckgo"},
		{"unset_with_endpoint", config.SearchConfig{SearXNG: config.SearXNGConfig{Endpoint: "http://x"}}, "searxng"},
		{"explicit_searxng_with_endpoint", config.SearchConfig{Provider: "searxng", SearXNG: config.SearXNGConfig{Endpoint: "http://x"}}, "searxng"},
		{"explicit_searxng_no_endpoint_fallback", config.SearchConfig{Provider: "searxng"}, "duckduckgo"},
		{"explicit_ddg", config.SearchConfig{Provider: "duckduckgo"}, "duckduckgo"},
		{"explicit_ddg_alias", config.SearchConfig{Provider: "ddg"}, "duckduckgo"},
		{"none", config.SearchConfig{Provider: "none"}, ""},
		{"off", config.SearchConfig{Provider: "off"}, ""},
		{"junk_falls_back", config.SearchConfig{Provider: "yahoo"}, "duckduckgo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := pickSearchBackend(tc.cfg)
			got := ""
			if b != nil {
				got = b.Name()
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRegisterSearch_NoneSuppresses(t *testing.T) {
	r := NewRegistry()
	RegisterSearch(r, config.SearchConfig{Provider: "none"})
	if tool := r.Get("web_search"); tool != nil {
		t.Errorf("expected web_search to be absent when provider=none, got %v", tool)
	}
}

func TestDecodeDDGURL(t *testing.T) {
	cases := map[string]string{
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2F&rut=x": "https://example.com/",
		"https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2F": "https://example.com/",
		"https://example.com/direct":                                  "https://example.com/direct",
		"":                                                            "",
	}
	for in, want := range cases {
		if got := decodeDDGURL(in); got != want {
			t.Errorf("decodeDDGURL(%q) = %q want %q", in, got, want)
		}
	}
}
