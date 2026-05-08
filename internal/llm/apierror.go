// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError carries a non-2xx HTTP response from the chat completions
// endpoint as a structured value. The chat-side error renderer
// type-asserts to this so it can show "rate-limited (429) · retry in
// 12s" instead of the generic "✘ HTTP 429: <body>" we emitted before.
//
// Body is capped at 1 KiB at decode time (see openai.go) so the value
// stays cheap to pass around. RetryAfter is non-zero only when the
// response carried a parseable Retry-After header — most commonly on
// 429s, occasionally on 503s.
type APIError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// IsRateLimited is true for 429 Too Many Requests. Surfaced as its own
// classification because the user response is "wait, then resubmit"
// — different from a 5xx where retrying immediately is fine.
func (e *APIError) IsRateLimited() bool { return e != nil && e.StatusCode == http.StatusTooManyRequests }

// IsServerError is true for 5xx. The model provider is broken; user
// can resubmit, but the underlying issue is upstream.
func (e *APIError) IsServerError() bool {
	return e != nil && e.StatusCode >= 500 && e.StatusCode < 600
}

// parseRetryAfter reads the Retry-After header per RFC 7231 §7.1.3.
// Two forms: delta-seconds (most common, all major LLM providers use
// this) and HTTP-date (rare). Returns zero on absent / unparseable
// headers — the renderer treats zero as "no countdown known" and
// shows just the badge.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}
