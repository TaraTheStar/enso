// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestOpenAI_MaxTokensInRequest verifies the cap is serialized as
// max_tokens when the client carries one, and omitted when it doesn't.
func TestOpenAI_MaxTokensInRequest(t *testing.T) {
	capture := func(c *OpenAIClient) string {
		var body string
		c.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(req.Body)
			body = string(b)
			return sseResponse("data: [DONE]\n\n"), nil
		})}
		events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		drainEvents(events)
		return body
	}

	with := capture(&OpenAIClient{Endpoint: "http://x", MaxTokens: 16384})
	if !strings.Contains(with, `"max_tokens":16384`) {
		t.Errorf("request body missing max_tokens:\n%s", with)
	}

	without := capture(&OpenAIClient{Endpoint: "http://x"})
	if strings.Contains(without, `"max_tokens"`) {
		t.Errorf("request body should omit max_tokens when unset:\n%s", without)
	}
}

// TestOpenAI_FinishReasonPropagated checks the provider's finish_reason
// rides on EventDone (previously dropped entirely).
func TestOpenAI_FinishReasonPropagated(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"length"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	c := &OpenAIClient{Endpoint: "http://x", HTTPClient: &http.Client{Transport: roundTripFunc(
		func(req *http.Request) (*http.Response, error) { return sseResponse(sse), nil })}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drainEvents(events)
	done := got[len(got)-1]
	if done.Type != EventDone {
		t.Fatalf("last event = %v, want EventDone", done.Type)
	}
	if done.FinishReason != FinishLength {
		t.Errorf("FinishReason = %q, want %q", done.FinishReason, FinishLength)
	}
}

// TestOpenAI_LoopGuardAborts feeds a degenerating stream and asserts the
// guard ends it with FinishRepetition rather than streaming forever.
func TestOpenAI_LoopGuardAborts(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, `data: {"choices":[{"index":0,"delta":{"content":"LOOP "}}]}`)
	}
	lines = append(lines, `data: [DONE]`, ``)
	sse := strings.Join(lines, "\n\n")

	c := &OpenAIClient{Endpoint: "http://x", LoopGuard: true,
		HTTPClient: &http.Client{Transport: roundTripFunc(
			func(req *http.Request) (*http.Response, error) { return sseResponse(sse), nil })}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drainEvents(events)
	done := got[len(got)-1]
	if done.FinishReason != FinishRepetition {
		t.Errorf("FinishReason = %q, want %q", done.FinishReason, FinishRepetition)
	}
	// Should have aborted well before draining all 200 deltas.
	var deltas int
	for _, e := range got {
		if e.Type == EventTextDelta {
			deltas++
		}
	}
	if deltas >= 200 {
		t.Errorf("loop guard didn't abort early: saw %d deltas", deltas)
	}
}

// TestOpenAI_LoopGuardOffStreamsFully confirms the guard is opt-in: with
// it disabled, a repetitive stream is delivered verbatim.
func TestOpenAI_LoopGuardOffStreamsFully(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, `data: {"choices":[{"index":0,"delta":{"content":"LOOP "}}]}`)
	}
	lines = append(lines, `data: [DONE]`, ``)
	sse := strings.Join(lines, "\n\n")

	c := &OpenAIClient{Endpoint: "http://x", LoopGuard: false,
		HTTPClient: &http.Client{Transport: roundTripFunc(
			func(req *http.Request) (*http.Response, error) { return sseResponse(sse), nil })}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drainEvents(events)
	var deltas int
	for _, e := range got {
		if e.Type == EventTextDelta {
			deltas++
		}
	}
	if deltas != 50 {
		t.Errorf("got %d deltas, want all 50 (guard off)", deltas)
	}
}

// TestOpenAI_StallTimeoutIgnoresPrefill verifies the watchdog does NOT
// arm before the first token: a long silent prefill (slow re-prefill on a
// hybrid-attention model at high context fill) followed by normal tokens
// must complete cleanly, not abort as a stall.
func TestOpenAI_StallTimeoutIgnoresPrefill(t *testing.T) {
	c := &OpenAIClient{Endpoint: "http://x", StallTimeout: 40 * time.Millisecond,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hdr := make(http.Header)
			hdr.Set("Content-Type", "text/event-stream")
			pr, pw := io.Pipe()
			go func() {
				// Silence well past StallTimeout BEFORE any token — this is
				// prefill, must not trip the watchdog.
				time.Sleep(120 * time.Millisecond)
				_, _ = io.WriteString(pw, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
				_, _ = io.WriteString(pw, "data: [DONE]\n\n")
				_ = pw.Close()
			}()
			return &http.Response{StatusCode: 200, Body: pr, Header: hdr}, nil
		})}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drainEvents(events)
	done := got[len(got)-1]
	if done.FinishReason == FinishStall {
		t.Error("watchdog tripped during prefill; should only guard inter-token gaps")
	}
	var sawText bool
	for _, e := range got {
		if e.Type == EventTextDelta && e.Text == "hello" {
			sawText = true
		}
	}
	if !sawText {
		t.Error("expected the post-prefill token to be delivered")
	}
}

// TestOpenAI_StallTimeoutAborts uses a body that emits one chunk then
// blocks forever; the watchdog must abort with FinishStall.
func TestOpenAI_StallTimeoutAborts(t *testing.T) {
	c := &OpenAIClient{Endpoint: "http://x", StallTimeout: 50 * time.Millisecond,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hdr := make(http.Header)
			hdr.Set("Content-Type", "text/event-stream")
			pr, pw := io.Pipe()
			go func() {
				_, _ = io.WriteString(pw, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
				// then never write again, never close — a hung stream.
			}()
			return &http.Response{StatusCode: 200, Body: pr, Header: hdr}, nil
		})}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	done := make(chan Event, 1)
	go func() {
		var last Event
		for e := range events {
			last = e
		}
		done <- last
	}()

	select {
	case last := <-done:
		if last.FinishReason != FinishStall {
			t.Errorf("FinishReason = %q, want %q", last.FinishReason, FinishStall)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stall watchdog did not fire; stream hung")
	}
}

// TestOpenAI_StallAbortDropsPartialToolCall streams a tool-call delta
// whose arguments JSON is cut off mid-stream, then hangs. The stall abort
// must NOT finalize the truncated call: the agent only runs maybeRecover
// on the no-tool-call path, so a finalized partial would both skip
// recovery and poison history with unparseable arguments.
func TestOpenAI_StallAbortDropsPartialToolCall(t *testing.T) {
	c := &OpenAIClient{Endpoint: "http://x", StallTimeout: 50 * time.Millisecond,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hdr := make(http.Header)
			hdr.Set("Content-Type", "text/event-stream")
			pr, pw := io.Pipe()
			go func() {
				_, _ = io.WriteString(pw, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls"}}]}}]}`+"\n\n")
				// then never write again — the arguments JSON stays truncated.
			}()
			return &http.Response{StatusCode: 200, Body: pr, Header: hdr}, nil
		})}}

	events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	done := make(chan []Event, 1)
	go func() { done <- drainEvents(events) }()

	var got []Event
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stall watchdog did not fire; stream hung")
	}

	last := got[len(got)-1]
	if last.Type != EventDone || last.FinishReason != FinishStall {
		t.Fatalf("last event = %+v, want EventDone with FinishReason %q", last, FinishStall)
	}
	for _, e := range got {
		if e.Type == EventToolCallComplete {
			t.Errorf("aborted stream finalized a partial tool call: %+v", e.ToolCalls)
		}
	}
}

// TestOpenAI_MalformedChunkDoesNotLeakParser feeds a stream whose first
// chunk is unparseable JSON with more chunks already buffered behind it.
// The decode-error path must drain sseDone so ParseSSE (parked on its
// next send) can exit — before the fix each such stream leaked the parser
// goroutine plus its scanner buffer permanently.
func TestOpenAI_MalformedChunkDoesNotLeakParser(t *testing.T) {
	sse := strings.Join([]string{
		`data: {not json`,
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	c := &OpenAIClient{Endpoint: "http://x", HTTPClient: &http.Client{Transport: roundTripFunc(
		func(req *http.Request) (*http.Response, error) { return sseResponse(sse), nil })}}

	baseline := runtime.NumGoroutine()
	const iterations = 20
	for i := 0; i < iterations; i++ {
		events, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		var sawErr bool
		for _, e := range drainEvents(events) {
			if e.Type == EventError {
				sawErr = true
			}
		}
		if !sawErr {
			t.Fatal("expected EventError for malformed chunk")
		}
	}

	// Leaked parsers never exit, so the count would stay pinned roughly
	// `iterations` above baseline; healthy teardown returns to (near)
	// baseline. Poll briefly — drainers finish asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() >= baseline+iterations/2 {
		if time.Now().After(deadline) {
			t.Fatalf("parser goroutines leaked: baseline %d, now %d after %d malformed streams",
				baseline, runtime.NumGoroutine(), iterations)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
