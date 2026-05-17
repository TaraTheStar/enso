// SPDX-License-Identifier: AGPL-3.0-or-later

package egress_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/TaraTheStar/enso/internal/backend/egress"
)

func TestProxyAllowlist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	p := egress.New()
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	proxyURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Default-deny: nothing on the allowlist yet.
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before allow, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Allow exactly the upstream host:port → now it passes through.
	u, _ := url.Parse(upstream.URL)
	p.Allow(u.Host)
	resp, err = client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request after allow: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("allowed request failed: %d %q", resp.StatusCode, body)
	}

	// A different host stays denied (allowlist is exact, not a bypass).
	resp, err = client.Get("http://198.51.100.1:9/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted host should be 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
