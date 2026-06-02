// SPDX-License-Identifier: AGPL-3.0-or-later

// Package egress is the host-side allowlist proxy for egress
// control. A network-sealed worker has no route out; when a task is
// granted a scoped egress capability (tier-3 broker), the granted
// host:port is added to this proxy's allowlist and the container is
// pointed at it via HTTPS_PROXY. Everything not explicitly allowed is
// refused — a curl-bash mishap or a wrong-remote push cannot leave the
// box. The allowlist is per-Proxy (one per task), starts empty
// (default-deny), and only ever grows by explicit Allow — except under
// --yolo, where AllowAll bypasses the gate entirely (traffic still flows
// through, and is observable at, this host proxy; the box stays sealed).
//
// An optional Decider turns the default-deny into an interactive prompt:
// on a not-allowed target the proxy asks (blocking) and, if granted,
// promotes the target to the allowlist. Without a Decider the deny is
// hard, exactly as before.
package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TaraTheStar/enso/internal/netsec"
)

// Proxy is an HTTP/HTTPS forward proxy that only connects to allowed
// host:port targets. HTTPS flows through CONNECT (the proxy never sees
// plaintext or terminates TLS — it is a policy gate, not a MITM); plain
// HTTP is forwarded with the same allowlist check.
// Decider is consulted when a target is NOT on the static allowlist
// (and allow-all is off). It blocks until the user (via the host-side
// InteractiveBroker) answers, returning true to allow the connection
// or false to refuse it. A nil Decider preserves the hard default-deny
// posture — an unconfigured sealed box still refuses everything.
//
// AuthorizeEgress runs on the request goroutine; ctx is the request
// context so a client that gives up unblocks the wait. An allowed
// target is also added to the allowlist (one decision per target, not
// per connection) before the proxy proceeds.
type Decider interface {
	AuthorizeEgress(ctx context.Context, hostport string) bool
}

type Proxy struct {
	mu       sync.RWMutex
	allowed  map[string]bool // "host:port", lowercased
	allowAll bool            // --yolo: every target permitted (no allowlist gate)
	decider  Decider         // optional interactive fallback on a denied target

	ln  net.Listener
	srv *http.Server
	tr  *http.Transport // plain-HTTP forward, dialing through safeDial
}

// denyIP is the SSRF address-class check, package-level so tests can swap
// it (e.g. permit loopback to point a denied target at an httptest
// server while keeping the other classes denied). Production uses the
// shared denylist in internal/netsec.
var denyIP = netsec.IsDeniedIP

// New creates an unstarted proxy with an empty (deny-all) allowlist.
func New() *Proxy { return &Proxy{allowed: map[string]bool{}} }

// Allow adds a host:port to the allowlist. Idempotent. A bare host (no
// ":port") allows that host on the conventional TLS/HTTP ports.
func (p *Proxy) Allow(hostport string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	hostport = strings.ToLower(strings.TrimSpace(hostport))
	if hostport == "" {
		return
	}
	if !strings.Contains(hostport, ":") {
		p.allowed[hostport+":443"] = true
		p.allowed[hostport+":80"] = true
		return
	}
	p.allowed[hostport] = true
}

// AllowAll switches the proxy to allow-all: every CONNECT/HTTP target is
// permitted and the per-target allowlist is bypassed. This is the --yolo
// posture — the box stays structurally sealed and all traffic still flows
// through (and is observable at) this host proxy, but the default-deny
// gate is off. Once set it is not unset for the proxy's lifetime.
func (p *Proxy) AllowAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowAll = true
}

// SetDecider installs the interactive fallback consulted on a target
// that is not (yet) on the allowlist. Idempotent; set once at wiring.
func (p *Proxy) SetDecider(d Decider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decider = d
}

func (p *Proxy) isAllowed(hostport string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowAll || p.allowed[strings.ToLower(hostport)]
}

// explicitlyAllowed reports whether hostport is on the static allowlist —
// WITHOUT the AllowAll short-circuit. This is the SSRF denylist's opt-out:
// a target the operator/broker named explicitly may resolve to an
// otherwise-denied address (the local-model-on-loopback case), exactly as
// web_fetch's allow_hosts exempts the same denylist. AllowAll (--yolo)
// does NOT exempt — its map is empty, so yolo traffic stays filtered and
// can't be relayed to loopback/metadata.
func (p *Proxy) explicitlyAllowed(hostport string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowed[strings.ToLower(hostport)]
}

// gate decides whether target may be reached: the static allowlist
// (or allow-all) first, then — only if a Decider is installed — an
// interactive prompt. A granted interactive decision is promoted to
// the allowlist so the rest of this connection and any retries to the
// same target don't re-prompt. No decider ⇒ pure default-deny.
func (p *Proxy) gate(ctx context.Context, target string) bool {
	if p.isAllowed(target) {
		return true
	}
	p.mu.RLock()
	d := p.decider
	p.mu.RUnlock()
	if d == nil {
		return false
	}
	if d.AuthorizeEgress(ctx, target) {
		p.Allow(target)
		return true
	}
	return false
}

// Allowed reports whether hostport is currently on the allowlist.
// Exported for diagnostics / status UI and broker-wiring assertions.
func (p *Proxy) Allowed(hostport string) bool {
	return p.isAllowed(hostport)
}

// Start binds a loopback listener and serves until Close. Addr is valid
// after a nil return.
func (p *Proxy) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("egress: listen: %w", err)
	}
	p.ln = ln
	p.tr = http.DefaultTransport.(*http.Transport).Clone()
	p.tr.DialContext = p.safeDial // pin resolved IPs; refuse denied classes
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle)}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

// safeDial resolves addr's host once, refuses the connection if ANY
// resolved IP is a denied class (loopback / RFC1918 / link-local /
// metadata / CGNAT / …), and then dials a surviving IP literal directly
// rather than letting the stack re-resolve. This is the DNS-rebind
// defence and, equally, what stops a sealed worker from using the proxy
// as an open relay into host-loopback or cloud-metadata services under
// --yolo (AllowAll), where the allowlist gate is off.
//
// The denylist runs on every dial EXCEPT for a target the operator/broker
// put on the explicit allowlist — that is the opt-out for the legitimate
// "reach my host-loopback model server" case, mirroring web_fetch's
// allow_hosts exemption. AllowAll does NOT exempt (its map is empty), so
// yolo traffic is still filtered. The "refuse if ANY IP is denied" stance
// (rather than "dial the first allowed IP") stops a name that resolves to
// a mix of public and loopback addresses from being used at all.
func (p *Proxy) safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("egress: bad target %q: %w", addr, err)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("egress: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: resolve %s: no addresses", host)
	}
	if !p.explicitlyAllowed(addr) {
		for _, ip := range ips {
			if denyIP(ip) {
				return nil, fmt.Errorf("egress: refusing %s: resolves to denied address %s", host, ip)
			}
		}
	}
	var lastErr error
	d := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("egress: no addresses to dial for %s", host)
	}
	return nil, lastErr
}

// Addr is the host:port the proxy listens on (loopback).
func (p *Proxy) Addr() string {
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// ProxyURL is the value to hand the container as HTTPS_PROXY.
func (p *Proxy) ProxyURL() string {
	if p.Addr() == "" {
		return ""
	}
	return "http://" + p.Addr()
}

// Close stops serving.
func (p *Proxy) Close() error {
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Plain HTTP: Host carries the target; default port 80.
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":80"
	}
	if !p.gate(r.Context(), target) {
		http.Error(w, "egress denied: "+target+" not on the task allowlist", http.StatusForbidden)
		return
	}
	r.RequestURI = ""
	resp, err := p.tr.RoundTrip(r)
	if err != nil {
		http.Error(w, "egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host // CONNECT host:port
	if !p.gate(r.Context(), target) {
		http.Error(w, "egress denied: "+target+" not on the task allowlist", http.StatusForbidden)
		return
	}
	dst, err := p.safeDial(r.Context(), "tcp", target)
	if err != nil {
		http.Error(w, "egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress: no hijack", http.StatusInternalServerError)
		_ = dst.Close()
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
	go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
}
