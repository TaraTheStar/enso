// SPDX-License-Identifier: AGPL-3.0-or-later

// Package seal generates the in-guest packet-layer egress firewall shared
// by the isolating backends (lima VM, podman container). The program is a
// default-deny OUTPUT chain — every NEW outbound connection the guest
// originates is REJECTed except loopback, the established/related return
// path (so the host's inbound control Channel keeps working), and, when a
// proxy is wired, exactly the host egress proxy at the backend gateway.
//
// It exists because pointing the guest at an HTTPS_PROXY env var is only
// advisory: a model that unsets the var (or dials by IP) egresses around
// the allowlist. The packet filter makes the proxy the box's ONLY route
// out at the network layer, not by convention. Both address families are
// covered — an IPv4-only chain leaves IPv6 OUTPUT open.
package seal

import "strings"

// Rules returns the iptables/ip6tables commands (no leading `set -e`; the
// caller wraps it in a `set -e` context so any failure aborts the seal —
// fail-closed). proxyHostport is the gateway host:port the guest reaches
// the host egress proxy on (IPv4); empty means a fully sealed box with no
// egress route at all. The proxy ACCEPT is v4-only since the gateway is
// IPv4; the v6 chain is a pure deny.
//
// Idempotent: it rebuilds its own ENSO_EGRESS chain and installs the
// OUTPUT jump at most once, so re-running it (e.g. a persistent VM across
// tasks with a fresh proxy port) is safe. `-w` waits for the xtables lock.
func Rules(proxyHostport string) string {
	allowProxy := ""
	if host, port, ok := splitHostPort(proxyHostport); ok {
		allowProxy = "iptables -w -A ENSO_EGRESS -p tcp -d " + host +
			" --dport " + port + " -j ACCEPT\n"
	}
	return denyChain("iptables", "icmp-port-unreachable", allowProxy) +
		denyChain("ip6tables", "icmp6-port-unreachable", "")
}

// splitHostPort splits "host:port" on the LAST colon (so IPv4 gateways and
// bracketless forms work) and reports false for empty input or a missing
// port — callers must not emit a port-less ACCEPT.
func splitHostPort(hostport string) (host, port string, ok bool) {
	if hostport == "" {
		return "", "", false
	}
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return "", "", false
	}
	host, port = hostport[:i], hostport[i+1:]
	if port == "" {
		return "", "", false
	}
	return host, port, true
}

// denyChain emits the idempotent ENSO_EGRESS default-deny program for one
// address family (cmd is iptables or ip6tables): allow loopback, allow the
// established/related return path, then any family-specific allowProxy
// line, then REJECT the rest, and finally install the OUTPUT jump once.
func denyChain(cmd, rejectWith, allowProxy string) string {
	return cmd + " -w -F ENSO_EGRESS 2>/dev/null || " + cmd + " -w -N ENSO_EGRESS\n" +
		cmd + " -w -A ENSO_EGRESS -o lo -j ACCEPT\n" +
		cmd + " -w -A ENSO_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n" +
		allowProxy +
		cmd + " -w -A ENSO_EGRESS -j REJECT --reject-with " + rejectWith + "\n" +
		cmd + " -w -C OUTPUT -j ENSO_EGRESS 2>/dev/null || " + cmd + " -w -I OUTPUT 1 -j ENSO_EGRESS\n"
}
