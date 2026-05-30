// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	if tr, err := searxngTransport(cfg.SearXNG); err != nil {
		// Bad ca_cert path/contents shouldn't crash startup — fall back
		// to the default transport and surface the error on first call
		// via TLS failure. Log once so it's not silent.
		fmt.Fprintf(os.Stderr, "enso: searxng TLS config: %v (falling back to default trust)\n", err)
	} else if tr != nil {
		client.Transport = tr
	}
	return &searxngBackend{
		endpoint:   strings.TrimRight(cfg.SearXNG.Endpoint, "/"),
		categories: strings.Join(cfg.SearXNG.Categories, ","),
		engines:    strings.Join(cfg.SearXNG.Engines, ","),
		apiKey:     cfg.SearXNG.APIKey,
		client:     client,
	}
}

// searxngTransport returns a custom http.Transport when the user has
// configured a ca_cert or insecure_skip_verify; nil means "use the
// default". ca_cert is appended to the system roots, not replacing
// them, so public CAs still verify.
//
// The CA bundle is preferentially taken from CACertPEM — bytes the host
// resolved via Config.ResolveSearchSecrets so a sealed worker (podman/
// lima, no host config dir mounted) needs no filesystem access. Only
// when those are absent (the local backend, where the path is valid in
// this very process) does it fall back to reading CACert by path.
func searxngTransport(cfg config.SearXNGConfig) (*http.Transport, error) {
	if cfg.CACert == "" && len(cfg.CACertPEM) == 0 && !cfg.InsecureSkipVerify {
		return nil, nil
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // opt-in via config
	if cfg.CACert != "" || len(cfg.CACertPEM) > 0 {
		pem := cfg.CACertPEM
		if len(pem) == 0 {
			b, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("read ca_cert %q: %w", cfg.CACert, err)
			}
			pem = b
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			src := fmt.Sprintf("%q", cfg.CACert)
			if cfg.CACert == "" {
				src = "(host-resolved)"
			}
			return nil, fmt.Errorf("ca_cert %s: no PEM certificates found", src)
		}
		tlsCfg.RootCAs = pool
	}
	// Clone the default transport rather than building a bare one so the
	// custom-TLS path keeps the default dial/idle timeouts, and route it
	// through envProxyFunc so HTTPS_PROXY is honoured. Without a Proxy the
	// HTTPS_PROXY injected by the sealed lima/podman backends is ignored,
	// the worker dials the search host directly, and the in-guest DNS lookup
	// is rejected by the egress firewall ("operation not permitted" on
	// udp :53).
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = envProxyFunc
	tr.TLSClientConfig = tlsCfg
	return tr, nil
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
