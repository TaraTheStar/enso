// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
)

func TestErrorBlockFromPayload_StringFallback(t *testing.T) {
	// Non-error payloads (or errors that aren't *llm.APIError) keep the
	// generic render — preserves existing UI for compaction failures,
	// permission denied, sandbox errors, etc.
	b := errorBlockFromPayload("kaboom")
	if b.apiErr != nil {
		t.Errorf("expected apiErr nil for plain string, got %v", b.apiErr)
	}
	if b.msg != "kaboom" {
		t.Errorf("msg=%q, want 'kaboom'", b.msg)
	}
}

func TestErrorBlockFromPayload_WrappedAPIError(t *testing.T) {
	// errors.As must find the *APIError even if wrapped (the agent
	// usually wraps with fmt.Errorf("chat: %w", err)).
	apiErr := &llm.APIError{StatusCode: 429, Body: "slow", RetryAfter: 5 * time.Second}
	b := errorBlockFromPayload(apiErr)
	if b.apiErr == nil {
		t.Fatal("apiErr should be non-nil")
	}
	if b.apiErr.StatusCode != 429 {
		t.Errorf("StatusCode=%d, want 429", b.apiErr.StatusCode)
	}
	if b.retryAt.IsZero() {
		t.Error("retryAt should be set when RetryAfter > 0")
	}
}

func TestApiErrorBadge_RateLimitedWithCountdown(t *testing.T) {
	b := &errorBlock{
		apiErr:  &llm.APIError{StatusCode: 429},
		retryAt: time.Now().Add(12 * time.Second),
	}
	got := apiErrorBadge(b)
	if !strings.Contains(got, "rate-limited (429)") {
		t.Errorf("missing rate-limited badge: %q", got)
	}
	if !strings.Contains(got, "retry in") {
		t.Errorf("missing 'retry in' countdown: %q", got)
	}
	if !strings.Contains(got, "[yellow]") {
		t.Errorf("rate-limited should be yellow: %q", got)
	}
}

func TestApiErrorBadge_RateLimitedExpired(t *testing.T) {
	b := &errorBlock{
		apiErr:  &llm.APIError{StatusCode: 429},
		retryAt: time.Now().Add(-time.Second),
	}
	got := apiErrorBadge(b)
	if !strings.Contains(got, "retry now") {
		t.Errorf("expired countdown should say 'retry now': %q", got)
	}
}

func TestApiErrorBadge_RateLimitedNoRetryAfter(t *testing.T) {
	// 429 without Retry-After header: still classified, no countdown.
	b := &errorBlock{apiErr: &llm.APIError{StatusCode: 429}}
	got := apiErrorBadge(b)
	if !strings.Contains(got, "rate-limited") {
		t.Errorf("missing rate-limited badge: %q", got)
	}
	if strings.Contains(got, "retry") {
		t.Errorf("should not show retry text without Retry-After: %q", got)
	}
}

func TestApiErrorBadge_ServerError(t *testing.T) {
	b := &errorBlock{apiErr: &llm.APIError{StatusCode: 502}}
	got := apiErrorBadge(b)
	if !strings.Contains(got, "provider error (502)") {
		t.Errorf("missing provider-error badge: %q", got)
	}
	if !strings.Contains(got, "[red]") {
		t.Errorf("provider error should be red: %q", got)
	}
}

func TestApiErrorBadge_OtherClientError(t *testing.T) {
	// 401, 400, etc. — neither rate-limit nor server error.
	b := &errorBlock{apiErr: &llm.APIError{StatusCode: 401}}
	got := apiErrorBadge(b)
	if !strings.Contains(got, "API error (401)") {
		t.Errorf("missing generic API-error badge: %q", got)
	}
}

func TestApiErrorExcerpt_OpenAIShape(t *testing.T) {
	body := `{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`
	got := apiErrorExcerpt(body)
	if got != "context length exceeded" {
		t.Errorf("excerpt=%q, want extracted error.message", got)
	}
}

func TestApiErrorExcerpt_RawFallback(t *testing.T) {
	// Body that isn't OpenAI-shaped JSON — show raw, collapsed,
	// truncated.
	got := apiErrorExcerpt("plain text error  spread\nover\nlines")
	if strings.Contains(got, "\n") {
		t.Errorf("excerpt should collapse newlines: %q", got)
	}
	if !strings.Contains(got, "plain text error") {
		t.Errorf("excerpt missing original text: %q", got)
	}
}

func TestApiErrorExcerpt_TruncatesLong(t *testing.T) {
	body := strings.Repeat("x", 200)
	got := apiErrorExcerpt(body)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long body should be truncated with ellipsis: %q", got)
	}
}

func TestApiErrorExcerpt_EmptyBody(t *testing.T) {
	if got := apiErrorExcerpt(""); got != "" {
		t.Errorf("empty body should produce empty excerpt, got %q", got)
	}
}

func TestEventError_RendersClassifiedBadge(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{
		Type:    bus.EventError,
		Payload: &llm.APIError{StatusCode: 429, Body: `{"error":{"message":"slow"}}`, RetryAfter: 30 * time.Second},
	})
	out := c.view.GetText(false)
	if !strings.Contains(out, "rate-limited (429)") {
		t.Errorf("missing classified badge in render: %q", out)
	}
	if !strings.Contains(out, "retry in") {
		t.Errorf("missing countdown in render: %q", out)
	}
	if !strings.Contains(out, "slow") {
		t.Errorf("missing extracted error message: %q", out)
	}
}

func TestEventError_PreservesGenericRenderForOtherErrors(t *testing.T) {
	// A non-APIError (compaction, permission, sandbox, etc.) must
	// still render via the existing ✘ path so we don't regress those
	// unrelated failure-block flows.
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventError, Payload: "compaction failed"})
	out := c.view.GetText(false)
	if !strings.Contains(out, "✘") {
		t.Errorf("missing generic ✘ prefix: %q", out)
	}
	if !strings.Contains(out, "compaction failed") {
		t.Errorf("missing original error text: %q", out)
	}
	if strings.Contains(out, "API error") || strings.Contains(out, "rate-limited") {
		t.Errorf("classified badge leaked into non-APIError render: %q", out)
	}
}

func TestHasLiveTimerBlock_TrueForActiveRetryAfter(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	// 429 with retry deadline 5s out — chat ticker must keep ticking
	// so the countdown advances each second.
	c.blocks = append(c.blocks, &errorBlock{
		apiErr:  &llm.APIError{StatusCode: 429},
		retryAt: time.Now().Add(5 * time.Second),
	})
	if !c.HasLiveTimerBlock() {
		t.Error("active 429 with future retryAt should request a tick")
	}
}

func TestHasLiveTimerBlock_FalseForExpiredRetryAfter(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.blocks = append(c.blocks, &errorBlock{
		apiErr:  &llm.APIError{StatusCode: 429},
		retryAt: time.Now().Add(-time.Second),
	})
	if c.HasLiveTimerBlock() {
		t.Error("expired retryAt should not request more ticks")
	}
}
