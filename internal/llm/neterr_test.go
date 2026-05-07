// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClassifyTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "connection refused",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{Err: syscall.ECONNREFUSED}, // wrapped via os.SyscallError
			},
			want: "connection refused",
		},
		{
			name: "dns",
			err:  &net.DNSError{Name: "nope.invalid", IsNotFound: true},
			want: "no such host (nope.invalid)",
		},
		{
			name: "host unreachable",
			err:  &net.OpError{Err: &os.SyscallError{Err: syscall.EHOSTUNREACH}},
			want: "host unreachable",
		},
		{
			name: "timeout via url.Error",
			err:  &url.Error{Op: "Post", URL: "http://x", Err: timeoutErr{}},
			want: "timed out",
		},
		{
			name: "unknown",
			err:  errors.New("some random thing"),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTransportError(tc.err)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFriendlyHTTPError_PreservesContextCancel(t *testing.T) {
	err := friendlyHTTPError("http://x", context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context.Canceled not preserved through friendlyHTTPError: %v", err)
	}
	err = friendlyHTTPError("http://x", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context.DeadlineExceeded not preserved: %v", err)
	}
}

func TestFriendlyHTTPError_ConnectErrorWraps(t *testing.T) {
	cause := &net.OpError{Err: &os.SyscallError{Err: syscall.ECONNREFUSED}}
	err := friendlyHTTPError("http://localhost:8080/v1", cause)

	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ConnectError, got %T: %v", err, err)
	}
	if ce.Endpoint != "http://localhost:8080/v1" {
		t.Errorf("endpoint not preserved: %q", ce.Endpoint)
	}
	if ce.Category != "connection refused" {
		t.Errorf("category: got %q want %q", ce.Category, "connection refused")
	}
	if !strings.Contains(ce.Error(), "couldn't reach") {
		t.Errorf("error message missing 'couldn't reach': %q", ce.Error())
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("syscall.ECONNREFUSED not reachable through Unwrap chain")
	}
}

func TestFriendlyHTTPError_UnknownPassthrough(t *testing.T) {
	cause := errors.New("some weird thing")
	err := friendlyHTTPError("http://x", cause)
	if !errors.Is(err, cause) {
		t.Errorf("cause not preserved through fall-through path: %v", err)
	}
	if !strings.Contains(err.Error(), "send request") {
		t.Errorf("expected 'send request' wrap, got: %q", err.Error())
	}
}

// timeoutErr satisfies net.Error.Timeout() == true for the url.Error
// timeout test. Stdlib doesn't export a convenient construct.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// --- compile-time assertions so signatures we depend on can't drift ---

var (
	_ net.Error         = timeoutErr{}
	_ http.RoundTripper = (*http.Transport)(nil)
	_ time.Duration     = 0
)
