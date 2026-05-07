// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// allowLoopback swaps denyHostFn so httptest's 127.0.0.1 server is
// reachable for the duration of t. The other denial classes still apply.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := denyHostFn
	denyHostFn = func(ip net.IP) bool {
		if ip.IsLoopback() {
			return false
		}
		return isDeniedIP(ip)
	}
	t.Cleanup(func() { denyHostFn = prev })
}

func TestWebFetchTool_PlainText(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	ac := newToolAC(t.TempDir())
	res, err := WebFetchTool{}.Run(context.Background(),
		map[string]any{"url": srv.URL}, ac)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "hello world") {
		t.Errorf("output = %q", res.LLMOutput)
	}
}

func TestWebFetchTool_StripsHTML(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h1>Title</h1><p>Body para.</p></body></html>"))
	}))
	defer srv.Close()

	ac := newToolAC(t.TempDir())
	res, err := WebFetchTool{}.Run(context.Background(),
		map[string]any{"url": srv.URL}, ac)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if strings.Contains(res.LLMOutput, "<h1>") || strings.Contains(res.LLMOutput, "</body>") {
		t.Errorf("HTML tags leaked into output:\n%s", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "Title") || !strings.Contains(res.LLMOutput, "Body para.") {
		t.Errorf("text fragments missing:\n%s", res.LLMOutput)
	}
}

func TestWebFetchTool_HTTPError(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ac := newToolAC(t.TempDir())
	res, err := WebFetchTool{}.Run(context.Background(),
		map[string]any{"url": srv.URL}, ac)
	if err != nil {
		t.Fatalf("fetch (transport ok, server returned 500): %v", err)
	}
	if !strings.Contains(res.LLMOutput, "HTTP 500") {
		t.Errorf("output = %q, want `HTTP 500`", res.LLMOutput)
	}
}

func TestWebFetchTool_RequiresURL(t *testing.T) {
	ac := newToolAC(t.TempDir())
	_, err := WebFetchTool{}.Run(context.Background(), map[string]any{}, ac)
	if err == nil {
		t.Errorf("missing url: want error")
	}
}

func TestIsDeniedIP(t *testing.T) {
	tests := []struct {
		ip   string
		deny bool
		why  string
	}{
		{"127.0.0.1", true, "loopback v4"},
		{"127.255.255.254", true, "loopback v4 range"},
		{"::1", true, "loopback v6"},
		{"10.0.0.1", true, "RFC1918"},
		{"172.20.0.1", true, "RFC1918"},
		{"192.168.1.1", true, "RFC1918"},
		{"169.254.169.254", true, "EC2 metadata"},
		{"fe80::1", true, "link-local v6"},
		{"fc00::1", true, "RFC4193 ULA"},
		{"100.64.0.1", true, "CGNAT"},
		{"100.127.255.255", true, "CGNAT high end"},
		{"100.63.255.255", false, "just below CGNAT"},
		{"100.128.0.0", false, "just above CGNAT"},
		{"0.0.0.0", true, "unspecified"},
		{"0.1.2.3", true, "this network 0/8"},
		{"255.255.255.255", true, "broadcast"},
		{"224.0.0.1", true, "multicast v4"},
		{"ff02::1", true, "multicast v6"},
		{"::", true, "unspecified v6"},
		{"8.8.8.8", false, "public dns"},
		{"1.1.1.1", false, "public dns"},
		{"2606:4700:4700::1111", false, "public v6"},
	}
	for _, tc := range tests {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", tc.ip)
		}
		got := isDeniedIP(ip)
		if got != tc.deny {
			t.Errorf("isDeniedIP(%s) = %v, want %v (%s)", tc.ip, got, tc.deny, tc.why)
		}
	}
}

func TestValidateAndResolve_BadScheme(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"gopher://x",
		"dict://x",
		"javascript:alert(1)",
		"",
		"not a url",
	}
	for _, c := range cases {
		if _, err := validateAndResolve(context.Background(), c, nil); err == nil {
			t.Errorf("scheme %q: want error, got nil", c)
		}
	}
}

func TestValidateAndResolve_RejectsCredentials(t *testing.T) {
	if _, err := validateAndResolve(context.Background(), "http://user:pass@example.com/x", nil); err == nil {
		t.Errorf("want error for embedded credentials")
	}
}

func TestValidateAndResolve_RejectsPrivateLiteralHost(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
	}
	for _, c := range cases {
		if _, err := validateAndResolve(context.Background(), c, nil); err == nil {
			t.Errorf("%s: expected refusal, got nil", c)
		}
	}
}

func TestValidateAndResolve_AllowHostExactMatch(t *testing.T) {
	allow := normaliseAllowHosts([]string{"127.0.0.1:8080"})
	if _, err := validateAndResolve(context.Background(), "http://127.0.0.1:8080/x", allow); err != nil {
		t.Errorf("allowed exact match failed: %v", err)
	}
	if _, err := validateAndResolve(context.Background(), "http://127.0.0.1:9090/x", allow); err == nil {
		t.Errorf("different port should not match exact entry")
	}
}

func TestValidateAndResolve_AllowHostWildcardPort(t *testing.T) {
	allow := normaliseAllowHosts([]string{"127.0.0.1"})
	for _, raw := range []string{"http://127.0.0.1/", "http://127.0.0.1:9999/y"} {
		if _, err := validateAndResolve(context.Background(), raw, allow); err != nil {
			t.Errorf("%s: wildcard-port allow should permit, got %v", raw, err)
		}
	}
}

func TestNormaliseAllowHosts_Shapes(t *testing.T) {
	m := normaliseAllowHosts([]string{
		"  ",
		"localhost",
		"127.0.0.1:8080",
		"[::1]:11434",
	})
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	wants := []string{"localhost:", "127.0.0.1:8080", "::1:11434"}
	for _, k := range wants {
		if !m[k] {
			t.Errorf("expected key %q in normalised map: got %v", k, m)
		}
	}
	if len(m) != len(wants) {
		t.Errorf("blank entry should be skipped: %v", m)
	}
}

// End-to-end: verify the redirect-to-private-IP guard fires by spinning
// up an httptest server (loopback allowed via override), pointing it at
// 169.254.169.254 — which the override still denies. The redirect must
// be refused.
func TestWebFetch_RedirectToMetadataBlocked(t *testing.T) {
	prev := denyHostFn
	denyHostFn = func(ip net.IP) bool {
		v4 := ip.To4()
		return v4 != nil && v4[0] == 169 && v4[1] == 254
	}
	t.Cleanup(func() { denyHostFn = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	tool := WebFetchTool{}
	res, err := tool.Run(context.Background(), map[string]interface{}{"url": srv.URL}, &AgentContext{})
	// Redirect refusals come back as Go errors from client.Do (CheckRedirect
	// returns an error). Either an error containing "refused" or a result
	// LLMOutput containing it is acceptable; what matters is that the
	// metadata IP wasn't actually fetched.
	got := res.LLMOutput
	if err != nil {
		got = err.Error()
	}
	if !strings.Contains(got, "refused") && !strings.Contains(got, "redirect") {
		t.Errorf("expected redirect refusal, got err=%v out=%q", err, res.LLMOutput)
	}
}
