// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WebFetchTool fetches a URL and returns text content. It refuses to
// reach into the user's local network: any URL whose host resolves to a
// loopback / RFC1918 / link-local / CGNAT / multicast / unspecified address
// is rejected before the dial. Without this guard the agent could probe
// 169.254.169.254 (cloud instance metadata), 127.0.0.1:* (the user's dev
// servers and the enso daemon), or RFC1918 LAN hosts. Specific local
// services can be opted back in via [web_fetch] allow_hosts in config.
//
// DNS-rebind defence: the resolved IP from validation is pinned for the
// actual TCP dial, so a hostname that resolves "public" at validation
// time can't switch to 127.0.0.1 between resolve and connect.
type WebFetchTool struct{}

func (t WebFetchTool) Name() string { return "web_fetch" }
func (t WebFetchTool) Description() string {
	return "Fetch a URL and return text content. Args: url (string). HTML tags are stripped; capped at 200KB. Refuses non-http(s) URLs and URLs that resolve to loopback / private / link-local addresses unless the host:port is in [web_fetch] allow_hosts."
}
func (t WebFetchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{"type": "string", "description": "URL to fetch"},
		},
		"required": []string{"url"},
	}
}

const (
	webFetchTimeout    = 30 * time.Second
	webFetchMaxBytes   = 200 * 1024
	webFetchMaxRedirs  = 10
	webFetchSummaryCap = 2000
)

func (t WebFetchTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	raw, _ := args["url"].(string)
	if raw == "" {
		return Result{}, fmt.Errorf("web_fetch: url required")
	}

	allow := normaliseAllowHosts(ac.WebFetchAllowHosts)
	pinned, err := validateAndResolve(ctx, raw, allow)
	if err != nil {
		return Result{LLMOutput: fmt.Sprintf("refused: %v", err)}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	client := newGuardedClient(pinned, allow)
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return Result{}, fmt.Errorf("web_fetch: %w", err)
	}
	req.Header.Set("User-Agent", "enso/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("web_fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		return Result{LLMOutput: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes))
	if err != nil {
		return Result{}, fmt.Errorf("web_fetch: read: %w", err)
	}

	content := string(data)

	ct := resp.Header.Get("Content-Type")
	isHTML := strings.Contains(ct, "text/html") || strings.HasPrefix(content, "<!") || strings.HasPrefix(content, "<html")
	var title string
	if isHTML {
		title = extractTitle(content)
		content = stripHTML(content)
	}

	truncated, full := HeadTail(content, webFetchSummaryCap)

	return Result{
		LLMOutput:     truncated,
		FullOutput:    full,
		DisplayOutput: webFetchDisplay(resp.StatusCode, len(data), ct, title),
	}, nil
}

// webFetchDisplay builds the one-line scrollback summary. The model
// gets the full extracted text via LLMOutput; this is purely what the
// user reads — they don't need to see 2000 chars of stripped HTML
// every time a page is fetched.
func webFetchDisplay(status, n int, contentType, title string) string {
	parts := []string{fmt.Sprintf("%d", status), humanBytes(n)}
	if title != "" {
		parts = append(parts, fmt.Sprintf("%q", title))
	} else if mt := mediaType(contentType); mt != "" {
		parts = append(parts, mt)
	}
	return strings.Join(parts, " · ")
}

// titleRe matches <title>…</title> case-insensitively, allowing
// attributes and surrounding whitespace. Non-greedy so a page with
// stray "</title>" elsewhere doesn't grab too much.
var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func extractTitle(html string) string {
	m := titleRe.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	t := strings.TrimSpace(m[1])
	// Collapse whitespace runs — multi-line titles render badly.
	t = strings.Join(strings.Fields(t), " ")
	const cap = 80
	if len(t) > cap {
		t = t[:cap-1] + "…"
	}
	return t
}

// mediaType strips ";charset=…" etc. off a Content-Type header.
func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

func stripHTML(s string) string {
	result := htmlTagRe.ReplaceAllString(s, "\n")
	result = strings.Join(strings.FieldsFunc(result, func(r rune) bool { return r == '\n' }), "\n")
	return strings.TrimSpace(result)
}

// pinnedHost records the original hostname plus the IPs that survived the
// SSRF check. The custom transport dials one of these addresses rather
// than re-resolving at TCP time (which is what makes DNS-rebind defeats
// fail). Port is the URL's port (default 80/443 by scheme).
type pinnedHost struct {
	host string
	port string
	ips  []net.IP
}

// validateAndResolve parses raw, enforces scheme + credential + host
// rules, resolves the host, and returns the pinned destination if every
// resolved IP is allowed (or the host:port is in allow). Returns an
// error suitable for surfacing to the model.
func validateAndResolve(ctx context.Context, raw string, allow map[string]bool) (*pinnedHost, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("malformed url")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported scheme %q (only http/https)", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("embedded credentials are not allowed in url")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url is missing host")
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}

	if !allowedHostPort(allow, host, port) {
		for _, ip := range ips {
			if denyHostFn(ip) {
				return nil, fmt.Errorf("%s resolves to disallowed address %s (add %q to [web_fetch] allow_hosts to permit)", host, ip, hostPortKey(host, port))
			}
		}
	}

	return &pinnedHost{host: host, port: port, ips: ips}, nil
}

// denyHostFn is package-level so tests can swap it (e.g. allow loopback to
// run an httptest server inside the redirect-blocked end-to-end test).
var denyHostFn = isDeniedIP

// isDeniedIP returns true for any address class the SSRF guard refuses.
// Covered: loopback, RFC1918 + RFC4193 ULA (IsPrivate), link-local
// (169.254/16 incl. EC2/Azure metadata, fe80::/10), CGNAT 100.64/10,
// 0.0.0.0/8, broadcast, multicast, unspecified.
func isDeniedIP(ip net.IP) bool {
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

// normaliseAllowHosts lowercases hosts and produces a map keyed by the
// canonical form so allowedHostPort can do O(1) lookups. Entries without
// a port are stored as "<host>:" (empty port slot) and match any port.
func normaliseAllowHosts(raw []string) map[string]bool {
	if len(raw) == 0 {
		return nil
	}
	m := make(map[string]bool, len(raw))
	for _, e := range raw {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		host, port, err := net.SplitHostPort(e)
		if err != nil {
			// no ":" present — treat as host with wildcard port
			host = e
			port = ""
		}
		host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
		m[host+":"+port] = true
	}
	return m
}

func allowedHostPort(allow map[string]bool, host, port string) bool {
	if len(allow) == 0 {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if allow[h+":"+port] {
		return true
	}
	if allow[h+":"] {
		return true
	}
	return false
}

func hostPortKey(host, port string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	return h + ":" + port
}

// newGuardedClient builds an *http.Client whose transport dials the
// pinned IP for the original host, and whose redirect handler re-runs
// validateAndResolve on every Location.
func newGuardedClient(pinned *pinnedHost, allow map[string]bool) *http.Client {
	dial := pinnedDialContext(pinned)
	tr := &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          1,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webFetchMaxRedirs {
				return fmt.Errorf("stopped after %d redirects", webFetchMaxRedirs)
			}
			next, err := validateAndResolve(req.Context(), req.URL.String(), allow)
			if err != nil {
				return fmt.Errorf("redirect to %s refused: %w", req.URL.Host, err)
			}
			// Swap dialer onto the new pinned host for the next hop.
			tr.DialContext = pinnedDialContext(next)
			return nil
		},
	}
}

// pinnedDialContext returns a DialContext that ignores the address
// supplied by the http stack and dials one of the pre-validated IPs on
// the pre-validated port. The Go http.Client uses the original URL.Host
// for SNI / TLS verification, so vhost / cert validation still work
// correctly against the original hostname.
func pinnedDialContext(pinned *pinnedHost) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var lastErr error
		d := net.Dialer{Timeout: 10 * time.Second}
		for _, ip := range pinned.ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), pinned.port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no addresses to dial")
		}
		return nil, lastErr
	}
}
