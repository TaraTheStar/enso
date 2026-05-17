// SPDX-License-Identifier: AGPL-3.0-or-later

// Package egress is the host-side allowlist proxy for egress
// control. A network-sealed worker has no route out; when a task is
// granted a scoped egress capability (tier-3 broker), the granted
// host:port is added to this proxy's allowlist and the container is
// pointed at it via HTTPS_PROXY. Everything not explicitly allowed is
// refused — a curl-bash mishap or a wrong-remote push cannot leave the
// box. The allowlist is per-Proxy (one per task), starts empty
// (default-deny), and only ever grows by explicit Allow.
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
)

// Proxy is an HTTP/HTTPS forward proxy that only connects to allowed
// host:port targets. HTTPS flows through CONNECT (the proxy never sees
// plaintext or terminates TLS — it is a policy gate, not a MITM); plain
// HTTP is forwarded with the same allowlist check.
type Proxy struct {
	mu      sync.RWMutex
	allowed map[string]bool // "host:port", lowercased

	ln  net.Listener
	srv *http.Server
}

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

func (p *Proxy) isAllowed(hostport string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allowed[strings.ToLower(hostport)]
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
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle)}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
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
	if !p.isAllowed(target) {
		http.Error(w, "egress denied: "+target+" not on the task allowlist", http.StatusForbidden)
		return
	}
	r.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(r)
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
	if !p.isAllowed(target) {
		http.Error(w, "egress denied: "+target+" not on the task allowlist", http.StatusForbidden)
		return
	}
	dst, err := net.DialTimeout("tcp", target, 10*time.Second)
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
