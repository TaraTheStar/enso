// SPDX-License-Identifier: AGPL-3.0-or-later

package egress

import (
	"context"
	"net"
)

// SetDenyIP swaps the SSRF address-class check and returns a restore
// func. Compiled only into the test binary (export_test.go), so it never
// widens the production API. Tests use it to treat loopback as a public
// stand-in (httptest servers bind 127.0.0.1) while keeping every other
// denied class active — and the SSRF test uses it the other way, pinning
// the real netsec denylist back to prove loopback is refused.
func SetDenyIP(f func(net.IP) bool) (restore func()) {
	old := denyIP
	denyIP = f
	return func() { denyIP = old }
}

// DialForTest exposes safeDial so the dialer's address-class refusals can
// be exercised without the HTTP plumbing. The conn is closed if a dial
// unexpectedly succeeds.
func DialForTest(ctx context.Context, target string) (net.Conn, error) {
	p := &Proxy{}
	c, err := p.safeDial(ctx, "tcp", target)
	if err == nil && c != nil {
		_ = c.Close()
	}
	return c, err
}
