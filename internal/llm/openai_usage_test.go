// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestOpenAI_StreamOptionsIncludeUsage_InRequest verifies that
// Chat() sends stream_options.include_usage=true in the request body
// — that's the opt-in OpenAI requires before it will emit a trailing
// usage chunk on the SSE stream.
func TestOpenAI_StreamOptionsIncludeUsage_InRequest(t *testing.T) {
	var capturedBody string
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		capturedBody = string(b)
		return sseResponse("data: [DONE]\n\n"), nil
	})
	c := &OpenAIClient{Endpoint: "http://x", HTTPClient: &http.Client{Transport: rt}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	drainEvents(events)

	if !strings.Contains(capturedBody, `"stream_options"`) {
		t.Errorf("request body missing stream_options:\n%s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"include_usage":true`) {
		t.Errorf("request body missing include_usage=true:\n%s", capturedBody)
	}
}

// TestOpenAI_UsageChunkEmitsEventUsage feeds a stream that ends in a
// trailing usage chunk (per OpenAI's documented shape) and asserts
// EventUsage is emitted with correctly-translated counts.
func TestOpenAI_UsageChunkEmitsEventUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49,"prompt_tokens_details":{"cached_tokens":10}}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return sseResponse(sse), nil
	})
	c := &OpenAIClient{Endpoint: "http://x", HTTPClient: &http.Client{Transport: rt}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drainEvents(events)

	var usageEvents []Event
	for _, e := range got {
		if e.Type == EventUsage {
			usageEvents = append(usageEvents, e)
		}
	}
	if len(usageEvents) != 1 {
		t.Fatalf("got %d EventUsage events, want exactly 1: %+v", len(usageEvents), got)
	}
	u := usageEvents[0].Usage
	if u.InputTokens != 42 || u.OutputTokens != 7 || u.TotalTokens != 49 {
		t.Errorf("usage = %+v, want in=42 out=7 total=49", u)
	}
	if u.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10 (from prompt_tokens_details.cached_tokens)", u.CacheReadTokens)
	}
}

// TestOpenAI_NoUsageWhenServerOmits verifies the graceful-degradation
// path: a server that doesn't honour stream_options (some llama.cpp
// builds) just doesn't send the trailing usage chunk, and the adapter
// emits no EventUsage rather than erroring.
func TestOpenAI_NoUsageWhenServerOmits(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return sseResponse(sse), nil
	})
	c := &OpenAIClient{Endpoint: "http://x", HTTPClient: &http.Client{Transport: rt}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drainEvents(events)

	for _, e := range got {
		if e.Type == EventUsage {
			t.Errorf("unexpected EventUsage when server omitted usage: %+v", e)
		}
	}
}

// helpers

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sseResponse(body string) *http.Response {
	hdr := make(http.Header)
	hdr.Set("Content-Type", "text/event-stream")
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     hdr,
	}
}

func drainEvents(ch <-chan Event) []Event {
	var out []Event
	for e := range ch {
		out = append(out, e)
	}
	return out
}
