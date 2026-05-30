// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"net/http"
	"net/url"

	"golang.org/x/net/http/httpproxy"
)

// envProxyFunc is an http.Transport.Proxy that reads the proxy environment
// (HTTPS_PROXY / HTTP_PROXY / NO_PROXY, upper- and lower-case) fresh on
// every call. Unlike http.ProxyFromEnvironment it does NOT cache the
// environment process-wide: a sealed worker (podman/lima) has the egress
// proxy injected at launch, so by request time it is always present, but
// reading fresh also keeps NO_PROXY honoured and lets tests toggle the env.
//
// Honouring this on every outbound HTTP transport in the tools package is
// what keeps web_fetch / web_search routed through the host egress proxy
// instead of dialing directly — a direct dial in a sealed guest forces an
// in-guest DNS lookup the firewall rejects ("operation not permitted" on
// udp :53).
func envProxyFunc(req *http.Request) (*url.URL, error) {
	return httpproxy.FromEnvironment().ProxyFunc()(req.URL)
}

// egressProxyForURL returns the egress proxy that applies to u, or nil if u
// should be dialed directly (no proxy configured, or u matches NO_PROXY).
// Same source of truth as envProxyFunc, used to pick the dial strategy
// before a request is built.
func egressProxyForURL(u *url.URL) *url.URL {
	p, err := httpproxy.FromEnvironment().ProxyFunc()(u)
	if err != nil {
		return nil
	}
	return p
}
