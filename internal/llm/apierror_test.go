// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func httpBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

func TestAPIError_Error(t *testing.T) {
	cases := []struct {
		err  *APIError
		want string
	}{
		{&APIError{StatusCode: 429, Body: "slow down"}, "HTTP 429: slow down"},
		{&APIError{StatusCode: 500, Body: ""}, "HTTP 500"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error()=%q, want %q", got, tc.want)
		}
	}
}

func TestAPIError_Classification(t *testing.T) {
	cases := []struct {
		status        int
		wantRateLimit bool
		wantServerErr bool
	}{
		{200, false, false},
		{401, false, false},
		{429, true, false},
		{500, false, true},
		{502, false, true},
		{599, false, true},
		{600, false, false}, // outside 5xx range
	}
	for _, tc := range cases {
		e := &APIError{StatusCode: tc.status}
		if got := e.IsRateLimited(); got != tc.wantRateLimit {
			t.Errorf("status=%d IsRateLimited=%v, want %v", tc.status, got, tc.wantRateLimit)
		}
		if got := e.IsServerError(); got != tc.wantServerErr {
			t.Errorf("status=%d IsServerError=%v, want %v", tc.status, got, tc.wantServerErr)
		}
	}
}

func TestParseRetryAfter_DeltaSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	got := parseRetryAfter(h)
	if got != 30*time.Second {
		t.Errorf("got %v, want 30s", got)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(45*time.Second).UTC().Format(http.TimeFormat))
	got := parseRetryAfter(h)
	// Allow 5s slack for test timing.
	if got < 40*time.Second || got > 50*time.Second {
		t.Errorf("got %v, want ~45s", got)
	}
}

func TestParseRetryAfter_Absent(t *testing.T) {
	if got := parseRetryAfter(http.Header{}); got != 0 {
		t.Errorf("absent header should return 0, got %v", got)
	}
}

func TestParseRetryAfter_NegativeOrPast(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "-1")
	if got := parseRetryAfter(h); got != 0 {
		t.Errorf("negative delta should clamp to 0, got %v", got)
	}
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if got := parseRetryAfter(h); got != 0 {
		t.Errorf("past HTTP-date should clamp to 0, got %v", got)
	}
}

func TestParseRetryAfter_Garbage(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "later")
	if got := parseRetryAfter(h); got != 0 {
		t.Errorf("unparseable header should return 0, got %v", got)
	}
}

func TestChat_ReturnsAPIErrorOn429(t *testing.T) {
	rt := &seqRT{steps: []seqStep{
		{resp: &http.Response{
			StatusCode: 429,
			Body:       httpBody(`{"error":{"message":"slow down"}}`),
			Header:     http.Header{"Retry-After": []string{"15"}},
		}},
	}}
	c := newTestClient(rt)
	c.Model = "test"

	_, err := c.Chat(t.Context(), ChatRequest{})
	if err == nil {
		t.Fatal("expected error on 429")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode=%d, want 429", apiErr.StatusCode)
	}
	if !apiErr.IsRateLimited() {
		t.Error("IsRateLimited should be true on 429")
	}
	if apiErr.RetryAfter != 15*time.Second {
		t.Errorf("RetryAfter=%v, want 15s", apiErr.RetryAfter)
	}
}
