// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
)

// searxngBackend talks to a single configured SearXNG instance via the
// JSON API (`/search?format=json`). The user picked the endpoint by
// putting it in their config, so there is no SSRF guard here — the
// host is trusted by definition.
type searxngBackend struct {
	endpoint   string
	categories string
	engines    string
	apiKey     string
	client     *http.Client
}

func newSearXNGBackend(cfg config.SearchConfig) *searxngBackend {
	timeout := cfg.SearXNG.Timeout
	if timeout <= 0 {
		timeout = defaultSearchTimeout
	}
	return &searxngBackend{
		endpoint:   strings.TrimRight(cfg.SearXNG.Endpoint, "/"),
		categories: strings.Join(cfg.SearXNG.Categories, ","),
		engines:    strings.Join(cfg.SearXNG.Engines, ","),
		apiKey:     cfg.SearXNG.APIKey,
		client:     &http.Client{Timeout: time.Duration(timeout) * time.Second},
	}
}

func (b *searxngBackend) Name() string { return "searxng" }

func (b *searxngBackend) Search(ctx context.Context, query string, n int) ([]SearchResult, error) {
	if b.endpoint == "" {
		return nil, fmt.Errorf("no endpoint configured")
	}

	u, err := url.Parse(b.endpoint + "/search")
	if err != nil {
		return nil, fmt.Errorf("bad endpoint: %w", err)
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	if b.categories != "" {
		q.Set("categories", b.categories)
	}
	if b.engines != "" {
		q.Set("engines", b.engines)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "enso/1.0")
	req.Header.Set("Accept", "application/json")
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			Engine        string `json:"engine"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	out := make([]SearchResult, 0, len(payload.Results))
	for _, r := range payload.Results {
		if r.URL == "" {
			continue
		}
		out = append(out, SearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Snippet:       r.Content,
			Engine:        r.Engine,
			PublishedDate: r.PublishedDate,
		})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}
