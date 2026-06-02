// SPDX-License-Identifier: AGPL-3.0-or-later

package seal

import (
	"strings"
	"testing"
)

func TestRules_DefaultDenyBothFamilies(t *testing.T) {
	// No proxy: a fully sealed box — lo + established only, REJECT the
	// rest, and NO port opened, for BOTH families.
	s := Rules("")
	for _, want := range []string{
		"iptables -w -A ENSO_EGRESS -o lo -j ACCEPT",
		"iptables -w -A ENSO_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"iptables -w -A ENSO_EGRESS -j REJECT --reject-with icmp-port-unreachable",
		"iptables -w -C OUTPUT -j ENSO_EGRESS",
		"ip6tables -w -A ENSO_EGRESS -o lo -j ACCEPT",
		"ip6tables -w -A ENSO_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"ip6tables -w -A ENSO_EGRESS -j REJECT --reject-with icmp6-port-unreachable",
		"ip6tables -w -C OUTPUT -j ENSO_EGRESS",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sealed rules missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--dport") {
		t.Errorf("no-proxy seal must open no port:\n%s", s)
	}
}

func TestRules_ProxyOpenedV4Only(t *testing.T) {
	s := Rules("10.0.2.2:54321")
	allow := "iptables -w -A ENSO_EGRESS -p tcp -d 10.0.2.2 --dport 54321 -j ACCEPT"
	if !strings.Contains(s, allow) {
		t.Errorf("proxied seal must open the gateway:port on v4:\n%s", s)
	}
	// ACCEPT must precede the v4 REJECT (order matters).
	if strings.Index(s, allow) > strings.Index(s, "iptables -w -A ENSO_EGRESS -j REJECT") {
		t.Errorf("proxy ACCEPT must precede REJECT:\n%s", s)
	}
	// The v6 chain must NOT open the port — the gateway is IPv4.
	v6 := s[strings.Index(s, "ip6tables"):]
	if strings.Contains(v6, "--dport") {
		t.Errorf("v6 chain must not open a port (gateway is IPv4):\n%s", s)
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in         string
		host, port string
		ok         bool
	}{
		{"10.0.2.2:54321", "10.0.2.2", "54321", true},
		{"192.168.5.2:80", "192.168.5.2", "80", true},
		{"", "", "", false},
		{"nocolon", "", "", false},
		{"host:", "", "", false}, // empty port → not ok (no port-less ACCEPT)
	}
	for _, c := range cases {
		h, p, ok := splitHostPort(c.in)
		if ok != c.ok || h != c.host || p != c.port {
			t.Errorf("splitHostPort(%q) = (%q,%q,%v); want (%q,%q,%v)", c.in, h, p, ok, c.host, c.port, c.ok)
		}
	}
}
