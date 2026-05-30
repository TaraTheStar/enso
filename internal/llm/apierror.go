// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"fmt"
	"net/http"
	"regexp"
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
func (e *APIError) IsRateLimited() bool {
	return e != nil && e.StatusCode == http.StatusTooManyRequests
}

// IsServerError is true for 5xx. The model provider is broken; user
// can resubmit, but the underlying issue is upstream.
func (e *APIError) IsServerError() bool {
	return e != nil && e.StatusCode >= 500 && e.StatusCode < 600
}

// IsContextOverflow reports whether this is a "prompt is larger than the
// model's context window" rejection — the recoverable case where the
// agent should compact and retry rather than dead-end the turn. Servers
// phrase it many ways (OpenAI's `context_length_exceeded` code, vLLM's
// "maximum context length is N tokens", litellm's "exceeds the available
// context size (N tokens)", llama.cpp's "exceeds the available context
// size"), so we match a small set of stable markers under a 400/413.
func (e *APIError) IsContextOverflow() bool {
	if e == nil {
		return false
	}
	if e.StatusCode != http.StatusBadRequest && e.StatusCode != http.StatusRequestEntityTooLarge {
		return false
	}
	b := strings.ToLower(e.Body)
	if strings.Contains(b, "context_length_exceeded") {
		return true
	}
	if !strings.Contains(b, "context") {
		return false
	}
	for _, marker := range []string{
		"exceeds the available context",
		"exceed the available context",
		"maximum context length",
		"context length",
		"context size",
		"context window",
	} {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}

// ctxLimitRe extracts the server-reported context window from an overflow
// error body. Anchored on the phrase that precedes the *limit* (not the
// requested count): "available context size (262144 tokens)", "maximum
// context length is 262144 tokens", "context window of 262144". The
// `\D{0,24}?` lazily skips punctuation/words like " (" or " is " between
// the phrase and the number.
var ctxLimitRe = regexp.MustCompile(`(?i)(?:available context size|maximum context length|context (?:length|size|window))\D{0,24}?(\d[\d,]*)\s*tokens`)

// ContextLimit returns the model's real context window in tokens, parsed
// from an overflow error body, when the server reported it. The agent
// adopts this as the effective window so future turns compact against the
// true limit instead of a missing or wrong configured value.
func (e *APIError) ContextLimit() (int, bool) {
	if e == nil {
		return 0, false
	}
	m := ctxLimitRe.FindStringSubmatch(e.Body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
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
