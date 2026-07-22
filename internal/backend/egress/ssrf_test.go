// SPDX-License-Identifier: AGPL-3.0-or-later

package egress_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/TaraTheStar/azoth/netsec"
	"github.com/TaraTheStar/enso/internal/backend/egress"
)

// allowLoopback permits 127.0.0.0/8 and ::1 while keeping every other
// denied class active. The egress proxy's whole job is dialing real
// (public) hosts; the tests use loopback httptest servers as a stand-in
// for those, so without this the SSRF guard added for S5 would refuse
// the stand-ins. The dedicated SSRF test below pins the REAL denylist
// back to prove loopback is genuinely refused in production.
func allowLoopback(ip net.IP) bool {
	if ip.IsLoopback() {
		return false
	}
	return netsec.IsDeniedIP(ip)
}

func TestMain(m *testing.M) {
	restore := egress.SetDenyIP(allowLoopback)
	code := m.Run()
	restore()
	os.Exit(code)
}

// TestProxySSRFGuard is the S5 regression. Two faces of the same denylist,
// mirroring web_fetch:
//   - AllowAll (--yolo) must NOT relay to a denied address (loopback /
//     RFC1918 / metadata / …) — that is the open-relay-into-host-internal
//     pivot the finding calls out. The yolo allowlist is empty, so the
//     denylist applies and the dial is refused.
//   - A RUNTIME grant (broker grant / interactive approval, Proxy.Allow)
//     opens the gate but does NOT exempt the denylist — the name is
//     worker-chosen, so DNS pointing it at loopback/metadata stays refused.
//   - An OPERATOR-CONFIGURED target (Proxy.AllowConfigured) IS exempt —
//     the operator named it in the config file, so a host-loopback model
//     server is reachable on purpose (same opt-out as web_fetch's
//     allow_hosts).
//
// These cases pin the REAL netsec denylist back (the test-wide TestMain
// default permits loopback so the legacy tests can use loopback upstreams).
func TestProxySSRFGuard(t *testing.T) {
	// A loopback upstream stands in for "a host-internal service".
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "host-internal")
	}))
	defer internal.Close()
	u, _ := url.Parse(internal.URL)

	newProxy := func(t *testing.T) (*egress.Proxy, *http.Client) {
		t.Helper()
		p := egress.New()
		if err := p.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		proxyURL, _ := url.Parse(p.ProxyURL())
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		return p, client
	}

	t.Run("AllowAll (--yolo) must not relay to loopback", func(t *testing.T) {
		defer egress.SetDenyIP(netsec.IsDeniedIP)()
		p, client := newProxy(t)
		p.AllowAll() // gate is off, but the denylist still applies

		resp, err := client.Get(internal.URL)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		// Gate allowed the name; the dial refused the loopback IP → 502.
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("AllowAll must not relay to loopback (want 502), got %d", resp.StatusCode)
		}
	})

	t.Run("runtime Allow must not exempt the denylist", func(t *testing.T) {
		defer egress.SetDenyIP(netsec.IsDeniedIP)()
		p, client := newProxy(t)
		p.Allow(u.Host) // broker/interactive grant: worker-chosen name → gate opens…

		resp, err := client.Get(internal.URL)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		// …but the dial still refuses the loopback IP → 502. A yolo broker
		// grant or an operator's "allow egress to foo" approval is a grant
		// for the NAME, never a waiver of private-address protection.
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("runtime-granted loopback target must still be refused (want 502), got %d", resp.StatusCode)
		}
	})

	t.Run("configured entry exempts the denylist (local-model case)", func(t *testing.T) {
		defer egress.SetDenyIP(netsec.IsDeniedIP)()
		p, client := newProxy(t)
		p.AllowConfigured(u.Host) // operator named THIS target in config → opt-out

		resp, err := client.Get(internal.URL)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "host-internal" {
			t.Fatalf("config-allowed loopback target must be reachable: %d %q", resp.StatusCode, body)
		}
	})
}

// TestSafeDialDeniesClasses exercises the dialer directly across the
// address classes, independent of the HTTP plumbing. This is the same
// denylist web_fetch pins; here it gates the proxy's own dial.
func TestSafeDialDeniesClasses(t *testing.T) {
	defer egress.SetDenyIP(netsec.IsDeniedIP)()

	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer internal.Close()
	loopHostPort := internal.Listener.Addr().String()

	for _, target := range []string{
		loopHostPort,         // 127.0.0.1:<port> live listener
		"169.254.169.254:80", // cloud metadata
		"10.0.0.1:443",       // RFC1918
		"[::1]:80",           // IPv6 loopback
		"100.64.0.1:80",      // CGNAT
	} {
		if _, err := egress.DialForTest(context.Background(), target); err == nil {
			t.Errorf("safeDial(%q) = nil error; want refusal", target)
		}
	}
}
