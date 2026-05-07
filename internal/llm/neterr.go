// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
)

// ConnectError wraps a transport-level failure from the LLM HTTP client
// with a friendly, actionable message. The agent loop and the CLI both
// surface error.Error() directly to the user, so a multi-line message is
// fine here.
type ConnectError struct {
	Endpoint string // base URL the request was aimed at
	Category string // short tag: "connection refused", "no such host", …
	Cause    error  // underlying error for `errors.Is` / `errors.Unwrap`
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf(
		"couldn't reach the chat endpoint at %s: %s\n"+
			"  is the server running and listening at that address?\n"+
			"  check `[providers.<name>] base_url` in your enso config\n"+
			"  (run `enso config show` to see config paths)",
		e.Endpoint, e.Category,
	)
}

func (e *ConnectError) Unwrap() error { return e.Cause }

// friendlyHTTPError inspects err for the common transport failures and
// returns a *ConnectError with a category tag if it can recognize one.
// Unknown errors fall through wrapped only with the request context so the
// caller's message stays informative.
func friendlyHTTPError(endpoint string, err error) error {
	if err == nil {
		return nil
	}

	// context cancellation isn't really a transport failure — preserve
	// the canonical sentinels so callers that check for them still see
	// what they expect.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	cat := classifyTransportError(err)
	if cat == "" {
		return fmt.Errorf("send request: %w", err)
	}
	return &ConnectError{Endpoint: endpoint, Category: cat, Cause: err}
}

func classifyTransportError(err error) string {
	// DNS lookup failure first — typo'd host, DNS down, etc.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "no such host (" + dnsErr.Name + ")"
	}

	// TLS handshake failures (Go 1.20+ surfaces these as a typed error;
	// earlier Go versions show up as a wrapped tls error).
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return "TLS certificate verification failed"
	}

	// syscall-level errors (refused, unreachable, reset, etc.).
	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNREFUSED:
			return "connection refused"
		case syscall.EHOSTUNREACH:
			return "host unreachable"
		case syscall.ENETUNREACH:
			return "network unreachable"
		case syscall.ECONNRESET:
			return "connection reset"
		}
	}

	// Timeouts wrapped in net.OpError / url.Error.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return "timed out"
	}

	return ""
}
