// SPDX-License-Identifier: AGPL-3.0-or-later

// Package netsec holds the shared SSRF denylist used wherever enso turns
// a model-supplied name into an outbound TCP connection. There are two
// such places: the in-guest web_fetch tool on the local backend (which
// resolve-and-pins) and the host-side egress proxy (which a sealed
// worker is pointed at via HTTPS_PROXY). Keeping the address-class
// classification in one place means a newly recognised dangerous range
// is closed on both paths at once.
package netsec

import "net"

// IsDeniedIP returns true for any address class an egress guard must
// refuse. Covered: loopback, RFC1918 + RFC4193 ULA (IsPrivate),
// link-local (169.254/16 incl. EC2/Azure metadata 169.254.169.254,
// fe80::/10), CGNAT 100.64/10, 0.0.0.0/8, broadcast, multicast,
// unspecified. A nil IP is denied (fail closed).
func IsDeniedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// CGNAT 100.64.0.0/10
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// "this network" 0.0.0.0/8 (IsUnspecified only catches the
		// single 0.0.0.0 address)
		if v4[0] == 0 {
			return true
		}
		if v4.Equal(net.IPv4bcast) {
			return true
		}
	}
	return false
}
