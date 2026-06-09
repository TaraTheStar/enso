// SPDX-License-Identifier: AGPL-3.0-or-later

package egress_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend/egress"
)

// fakeDecider records how many times it was consulted and answers with
// a fixed verdict — stands in for the host InteractiveBroker.
type fakeDecider struct {
	allow bool
	calls atomic.Int32
}

func (d *fakeDecider) AuthorizeEgress(_ context.Context, _ string) bool {
	d.calls.Add(1)
	return d.allow
}

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

// TestProxyConnectTunnel proves the HTTPS (CONNECT) path end-to-end: a
// real TLS round-trip flows through the proxy's tunnel relay, and the
// tunnel's two relay goroutines are released after the connection closes
// (rather than leaking fds — the failure that makes the proxy eventually
// 502 everything). httptest's TLS server binds loopback, so the target is
// added to the explicit allowlist, which is also the SSRF-denylist opt-out
// that permits dialing 127.0.0.1.
func TestProxyConnectTunnel(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tunnel-ok")
	}))
	defer upstream.Close()

	p := egress.New()
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	u, _ := url.Parse(upstream.URL)
	p.Allow(u.Host) // explicit allow == loopback SSRF opt-out

	proxyURL, _ := url.Parse(p.ProxyURL())
	// Reuse the TLS server's client (trusts its self-signed cert), but
	// route through the proxy so the request goes out as CONNECT.
	client := upstream.Client()
	tr := client.Transport.(*http.Transport)
	tr.Proxy = http.ProxyURL(proxyURL)

	base := runtime.NumGoroutine()

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("https request through CONNECT: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "tunnel-ok" {
		t.Fatalf("tunneled request failed: %d %q", resp.StatusCode, body)
	}

	// Drop the client's idle TLS conn; the proxy's two relay goroutines
	// must then return (each Read unblocks once both conns are closed),
	// not leak. Poll because teardown is asynchronous.
	tr.CloseIdleConnections()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - base; leaked > 2 {
		t.Fatalf("tunnel goroutines leaked: %d still live above baseline", leaked)
	}
}

// TestProxyStripsHopByHop proves the proxy does not forward connection-
// scoped headers in either direction — leaking them desyncs keep-alive
// framing on a reused client connection (the "first request works, the
// next cancels" failure). The upstream echoes back which hop-by-hop
// headers it received, and the client inspects the response headers.
func TestProxyStripsHopByHop(t *testing.T) {
	var gotUpstream http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpstream = r.Header.Clone()
		// Set hop-by-hop headers on the RESPONSE; the proxy must strip
		// these before handing the response back to the client.
		w.Header().Set("Connection", "X-Custom-Hop")
		w.Header().Set("X-Custom-Hop", "secret")
		w.Header().Set("Keep-Alive", "timeout=5")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	p := egress.New()
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()
	u, _ := url.Parse(upstream.URL)
	p.Allow(u.Host)

	proxyURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Connection", "X-Drop-Me")
	req.Header.Set("X-Drop-Me", "1")
	req.Header.Set("Keep-Alive", "timeout=5")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// Request side: the upstream must not have seen the hop-by-hop headers.
	if v := gotUpstream.Get("X-Drop-Me"); v != "" {
		t.Errorf("hop-by-hop request header leaked upstream: X-Drop-Me=%q", v)
	}
	if v := gotUpstream.Get("Keep-Alive"); v != "" {
		t.Errorf("Keep-Alive leaked upstream: %q", v)
	}
	// Response side: the client must not receive the upstream's hop-by-hop
	// headers (the Connection-listed X-Custom-Hop and Keep-Alive).
	if v := resp.Header.Get("X-Custom-Hop"); v != "" {
		t.Errorf("Connection-listed response header leaked to client: %q", v)
	}
	if v := resp.Header.Get("Keep-Alive"); v != "" {
		t.Errorf("Keep-Alive leaked to client: %q", v)
	}
}

// TestProxyAllowAll proves the --yolo posture: AllowAll lifts the
// default-deny gate so a never-Allow'd host passes, while traffic still
// goes through the proxy (the box stays sealed; this is its only route).
func TestProxyAllowAll(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "yolo-ok")
	}))
	defer upstream.Close()

	p := egress.New()
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.Close()

	proxyURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Before AllowAll: default-deny still in force (nothing Allow'd).
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before AllowAll, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// After AllowAll: the exact same never-allowlisted host passes.
	p.AllowAll()
	resp, err = client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("request after AllowAll: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "yolo-ok" {
		t.Fatalf("AllowAll request failed: %d %q", resp.StatusCode, body)
	}
	if !p.Allowed("anything.example:443") {
		t.Fatal("AllowAll must report every target as allowed")
	}
}

// TestProxyDecider proves the interactive fallback: a not-allowlisted
// target consults the Decider; a grant passes AND promotes the target
// (so the next request to it does not re-ask); a refusal 403s.
func TestProxyDecider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	t.Run("grant promotes target", func(t *testing.T) {
		p := egress.New()
		if err := p.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		defer p.Close()
		d := &fakeDecider{allow: true}
		p.SetDecider(d)

		proxyURL, _ := url.Parse(p.ProxyURL())
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

		for i := 0; i < 2; i++ {
			resp, err := client.Get(upstream.URL)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(body) != "ok" {
				t.Fatalf("granted request %d failed: %d %q", i, resp.StatusCode, body)
			}
		}
		// One decision per target, not per request: the grant was
		// promoted to the allowlist.
		if got := d.calls.Load(); got != 1 {
			t.Fatalf("decider must be consulted once per target, got %d calls", got)
		}
		if !p.Allowed(u.Host) {
			t.Error("granted target must be promoted to the allowlist")
		}
	})

	t.Run("refusal 403s and does not promote", func(t *testing.T) {
		p := egress.New()
		if err := p.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		defer p.Close()
		d := &fakeDecider{allow: false}
		p.SetDecider(d)

		proxyURL, _ := url.Parse(p.ProxyURL())
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

		resp, err := client.Get(upstream.URL)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("refused egress must 403, got %d", resp.StatusCode)
		}
		if p.Allowed(u.Host) {
			t.Error("refused target must NOT be on the allowlist")
		}
	})
}
