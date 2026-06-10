// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Debug logging.
//
// We write to a file rather than stderr because stderr in a tview-driven
// terminal session interleaves with the TUI's own draw calls and visibly
// corrupts the screen. Tail the file in another pane during diagnosis.
//
// Toggled via the `--debug` CLI flag (which calls SetDebug from
// PersistentPreRunE) or by leaving ENSO_DEBUG set in the environment.
var (
	debugMu     sync.RWMutex
	debugWriter io.Writer = io.Discard
)

// SetDebug enables debug-log writing to `path`. Pass "" to disable.
// Idempotent and concurrency-safe.
func SetDebug(path string) error {
	if path == "" {
		debugMu.Lock()
		debugWriter = io.Discard
		debugMu.Unlock()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	// debug.log captures full chat request bodies (system prompt, user
	// turns, tool args) — clamp the parent dir mode on every enable in
	// case it predates the 0700 tightening.
	_ = os.Chmod(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	debugMu.Lock()
	debugWriter = f
	debugMu.Unlock()
	return nil
}

func debugLog() io.Writer {
	debugMu.RLock()
	defer debugMu.RUnlock()
	return debugWriter
}

// EventType identifies the kind of streaming event.
type EventType int

const (
	EventTextDelta EventType = iota
	EventReasoningDelta
	EventToolCallComplete
	// EventUsage carries provider-reported token counts for the turn
	// that's about to finish. Adapters emit at most one per Chat()
	// call, after the final content/tool-call events and before
	// EventDone. Consumers that don't track usage may safely ignore
	// it — Type checks default-fall-through.
	EventUsage
	EventDone
	EventError
)

// Event is a single item emitted by the streaming Chat loop.
//
// Usage is populated only when Type == EventUsage; for all other
// kinds it carries the zero value and should not be inspected.
type Event struct {
	Type      EventType
	Text      string
	ToolCalls []ToolCall
	Usage     MessageUsage
	Error     error
	// FinishReason is set only on EventDone. It carries the provider's
	// reported reason ("stop", "length", "tool_calls") plus two synthetic
	// values the OpenAI adapter raises itself: "repetition" when the loop
	// guard tripped and "stall" when the stream went silent past the
	// configured timeout. The agent uses it to drive auto-recovery; other
	// adapters leave it "" and recovery is a no-op for them.
	FinishReason string
}

// Finish-reason values the OpenAI adapter raises beyond the provider's
// own "stop" / "length" / "tool_calls". Kept as constants so the agent's
// recovery switch and the adapter stay in lockstep.
const (
	FinishLength     = "length"     // provider hit the max_tokens cap
	FinishRepetition = "repetition" // loop guard tripped mid-stream
	FinishStall      = "stall"      // no token for StallTimeout
	// FinishReasoningBudget: the model streamed more reasoning
	// (chain-of-thought) than ReasoningBudget without starting to act —
	// no answer text and no tool call. Aborted so the agent can nudge it
	// to commit instead of deliberating toward the max_tokens cap.
	FinishReasoningBudget = "reasoning_budget"
)

// ChatClient is the streaming-chat interface the agent and compaction
// loop use. *OpenAIClient is one production implementation (alongside
// other vendor adapters like *AnthropicClient); tests substitute a fake
// via the llmtest package.
type ChatClient interface {
	Chat(ctx context.Context, req ChatRequest) (<-chan Event, error)
}

// OpenAIClient wraps one OpenAI-compatible HTTP endpoint.
type OpenAIClient struct {
	Endpoint string
	APIKey   string
	Model    string

	// MaxTokens caps a single generation (sent as max_tokens / n_predict).
	// Zero means "send no cap" — the server's own default applies. Set by
	// provider.go from resolved config (explicit value, else derived from
	// the context window). The hard backstop against runaway generation.
	MaxTokens int

	// StallTimeout aborts a stream that produces no token for this long.
	// Rate-independent by design: it fires on silence, not slowness, so
	// the same value works across a fast GPU and a slow large-context box.
	// Zero disables the watchdog.
	StallTimeout time.Duration

	// LoopGuard enables mid-stream repetition detection — when the tail of
	// the output collapses into a short repeating cycle, the stream is
	// aborted with FinishRepetition so the agent can recover before the
	// max_tokens cap is even reached.
	LoopGuard bool

	// ReasoningBudget caps how many reasoning (chain-of-thought) runes a
	// model may stream before it starts acting — emitting answer text or a
	// tool call. Exceeding it aborts the stream with FinishReasoningBudget
	// so the agent can nudge the model to commit. Targets the local-model
	// failure mode where a reasoning model deliberates for minutes without
	// deciding. Zero disables it (the default); the novelty loop guard is
	// the primary defense and this is an opt-in hard backstop.
	ReasoningBudget int

	// HTTPClient is the transport used for both /chat/completions and
	// the recovery probe. nil means http.DefaultClient — production
	// leaves it unset; tests inject a custom RoundTripper to drive
	// retry/probe state transitions deterministically.
	HTTPClient *http.Client

	// RetryBackoff and ProbeInterval are test seams. Production leaves
	// both nil/zero and falls back to the package defaults
	// (retryBackoff / probeInterval). Tests use tight values so a full
	// retry-and-probe cycle finishes in milliseconds.
	RetryBackoff  func(attempt int) time.Duration
	ProbeInterval time.Duration

	// conn tracks the last-known transport state for this endpoint so
	// the TUI can render a "reconnecting / disconnected" indicator. Only
	// transport-class errors (DNS, refused, reset, timeout, …) move the
	// state — HTTP-status errors leave it Connected because TLS+TCP did
	// succeed.
	conn connTracker
}

func (c *OpenAIClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// LLMConnState satisfies ConnStateReporter. Returning by value keeps the
// reader path lock-light — callers don't need to hold any reference to
// the tracker itself.
func (c *OpenAIClient) LLMConnState() ConnState { return c.conn.get() }

// retryBackoff returns the wait between attempt N and attempt N+1. Two
// retries (500ms, 1.5s) cover the vast majority of transient blips —
// DHCP renews, restart of a local llama.cpp, momentary upstream 502s on
// a hosted API — without piling up wait time when the endpoint is
// genuinely down.
func retryBackoff(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 500 * time.Millisecond
	case 1:
		return 1500 * time.Millisecond
	default:
		return 3 * time.Second
	}
}

// maxChatRetries is the number of *additional* attempts after the
// initial request fails on a transport error. Total request budget is
// maxChatRetries + 1.
const maxChatRetries = 2

// doChatRequest performs the HTTP request with transport-only retry.
// Non-transport errors (HTTP status, body marshal, context cancel) bypass
// the retry loop and surface immediately — only network-class failures
// are worth retrying.
func (c *OpenAIClient) doChatRequest(ctx context.Context, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxChatRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}

		resp, err := c.httpClient().Do(req)
		if err == nil {
			return resp, nil
		}

		// User-cancellation isn't a transport problem — surface as-is so
		// callers using errors.Is(ctx.Err()) still match.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}

		// Non-transport errors don't benefit from retry.
		if classifyTransportError(err) == "" {
			return nil, friendlyHTTPError(c.Endpoint, err)
		}

		lastErr = err
		// We're going to retry — surface that on the indicator before
		// the user-visible delay starts. Probe goroutine isn't spawned
		// here; that only happens once retries are exhausted.
		c.conn.set(StateReconnecting)

		if attempt == maxChatRetries {
			break
		}
		backoff := retryBackoff
		if c.RetryBackoff != nil {
			backoff = c.RetryBackoff
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return nil, friendlyHTTPError(c.Endpoint, lastErr)
}

// Chat sends a streaming chat completion request and yields events on the returned channel.
func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	req.Stream = true
	req.Model = c.Model
	req.Messages = FilterForRequest(req.Messages)
	if c.MaxTokens > 0 {
		req.MaxTokens = c.MaxTokens
	}

	// Wrap with stream_options.include_usage=true so the provider sends
	// a trailing usage chunk we can translate to EventUsage. OpenAI and
	// vLLM honour this; servers that don't (some llama.cpp builds) treat
	// it as an unknown field and ignore it — we'll fall back to the
	// heuristic estimate on the agent side when no usage arrives.
	type streamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	}
	wrapped := struct {
		ChatRequest
		StreamOptions streamOptions `json:"stream_options"`
	}{ChatRequest: req, StreamOptions: streamOptions{IncludeUsage: true}}

	data, err := json.Marshal(wrapped)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	fmt.Fprintf(debugLog(), "POST %s/chat/completions\nbody: %s\n", c.Endpoint, string(data))

	resp, err := c.doChatRequest(ctx, data)
	if err != nil {
		// All retries (if any) exhausted on a transport error: flip the
		// indicator to Disconnected and start the recovery probe so the
		// TUI clears the marker once the endpoint is back, even if the
		// user never sends another turn.
		if classifyTransportError(err) != "" {
			if c.conn.set(StateDisconnected) != StateDisconnected {
				c.startRecoveryProbe()
			}
		}
		return nil, err
	}

	// We got *some* HTTP response — TCP+TLS were healthy enough. Even a
	// 500 means the endpoint is reachable, so clear any prior degraded
	// state. (HTTP-error rendering is handled below / by callers.)
	c.conn.set(StateConnected)

	if resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		retryAfter := parseRetryAfter(resp.Header)
		resp.Body.Close()
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
			RetryAfter: retryAfter,
		}
	}

	eventCh := make(chan Event, 32)

	go func() {
		defer close(eventCh)
		defer resp.Body.Close()

		acc := NewToolCallAccumulator()
		sseDone := make(chan []byte)
		var sseErr error
		go ParseSSE(resp.Body, sseDone, &sseErr)

		var detector *repetitionDetector
		if c.LoopGuard {
			detector = newRepetitionDetector()
		}

		var lastUsage *ChunkUsage
		finishReason := ""

		// drainSSE lets ParseSSE finish any pending send and exit instead
		// of leaking. The parser is typically parked on `done <- data`
		// (with later chunks already buffered in its scanner), and the
		// deferred resp.Body.Close() does NOT unblock a channel send — so
		// every path that stops reading sseDone before it closes must
		// drain it.
		drainSSE := func() {
			go func() {
				for range sseDone {
				}
			}()
		}

		// abort tears down a stream we've decided to stop reading early
		// (stall watchdog, loop guard, or reasoning budget). Closing the
		// body unblocks ParseSSE's in-flight Read; draining sseDone lets it
		// finish any pending send and exit. First reason wins.
		aborted := false
		abort := func(reason string) {
			if aborted {
				return
			}
			aborted = true
			finishReason = reason
			resp.Body.Close()
			drainSSE()
		}

		// started flips once the first chunk arrives. The stall watchdog
		// only guards the gap BETWEEN chunks, never the initial wait — a
		// hybrid-attention model (Qwen3.6 etc.) re-prefills the whole
		// prompt each turn and at a high context fill that prefill can run
		// far longer than StallTimeout while emitting nothing. Arming
		// before the first token would kill those healthy turns. Prefill
		// can take as long as it takes; once we're decoding, a long
		// silence genuinely means hung. Rate-independent: keys off
		// inter-token silence, not throughput.
		started := false

		// reasoningRunes counts chain-of-thought emitted before the model
		// starts acting; acting flips true on the first content or tool-call
		// delta, after which the budget no longer applies (the thinking
		// phase is over and a long answer is legitimate).
		reasoningRunes := 0
		acting := false
	stream:
		for {
			var raw []byte
			var ok bool
			if c.StallTimeout > 0 && started {
				timer := time.NewTimer(c.StallTimeout)
				select {
				case raw, ok = <-sseDone:
					timer.Stop()
				case <-timer.C:
					abort(FinishStall)
					break stream
				}
			} else {
				raw, ok = <-sseDone
			}
			if !ok {
				break
			}
			started = true

			fmt.Fprintf(debugLog(), "sse: %s\n", string(raw))
			var chunk ChatCompletionChunk
			if err := json.Unmarshal(raw, &chunk); err != nil {
				drainSSE()
				eventCh <- Event{Type: EventError, Error: fmt.Errorf("unmarshal chunk: %w", err)}
				return
			}

			for _, ch := range chunk.Choices {
				if ch.FinishReason != "" {
					finishReason = ch.FinishReason
				}
				delta := ch.Delta
				if delta.ReasoningContent != "" {
					eventCh <- Event{Type: EventReasoningDelta, Text: delta.ReasoningContent}
					if detector != nil && detector.addReasoning(delta.ReasoningContent) {
						abort(FinishRepetition)
						break stream
					}
					if c.ReasoningBudget > 0 && !acting {
						for range delta.ReasoningContent {
							reasoningRunes++
						}
						if reasoningRunes > c.ReasoningBudget {
							abort(FinishReasoningBudget)
							break stream
						}
					}
				}
				if delta.Content != "" {
					acting = true
					eventCh <- Event{Type: EventTextDelta, Text: delta.Content}
					if detector != nil && detector.add(delta.Content) {
						abort(FinishRepetition)
						break stream
					}
				}
				if len(delta.ToolCalls) > 0 {
					acting = true
					if err := acc.Merge(delta); err != nil {
						drainSSE()
						eventCh <- Event{Type: EventError, Error: fmt.Errorf("merge tool call: %w", err)}
						return
					}
				}
			}
			// OpenAI sends usage on the trailing chunk (Choices empty).
			// vLLM matches the shape; non-supporting servers leave Usage
			// nil and we silently fall back to heuristic on the agent
			// side.
			if chunk.Usage != nil {
				lastUsage = chunk.Usage
			}
		}

		// A scan/read error (truncated body, or a line past maxSSELineBytes)
		// must NOT masquerade as a clean finish — otherwise a partial
		// assistant message gets persisted as final. Skip when we tore the
		// stream down ourselves (abort closes resp.Body, which surfaces as a
		// read error that's expected, not a fault). The channel close
		// happens-after ParseSSE's write to sseErr, so reading it here is safe.
		if !aborted && sseErr != nil {
			eventCh <- Event{Type: EventError, Error: fmt.Errorf("sse stream: %w", sseErr)}
			return
		}

		// An aborted stream (stall / repetition / reasoning budget) may have
		// cut a tool call off mid-arguments. Finalizing it would hand the
		// agent a call with truncated JSON — and any tool call at all makes
		// agent.turn skip maybeRecover, so the abort reason would be
		// discarded and the corrupted call persisted into history. Drop the
		// partial instead: the turn reaches the agent with no tool calls and
		// goes through the designed retry-with-nudge recovery.
		if !aborted {
			if calls := acc.Finalize(); len(calls) > 0 {
				eventCh <- Event{Type: EventToolCallComplete, ToolCalls: calls}
				if finishReason == "" {
					finishReason = "tool_calls"
				}
			}
		}

		if lastUsage != nil {
			usage := MessageUsage{
				InputTokens:  lastUsage.PromptTokens,
				OutputTokens: lastUsage.CompletionTokens,
				TotalTokens:  lastUsage.TotalTokens,
			}
			// OpenAI counts cached_tokens as a sub-line of prompt_tokens
			// (not additive). Surface separately so downstream
			// observability can show cache hit ratio, but don't add to
			// TotalTokens — that would double-count.
			if lastUsage.PromptTokensDetails != nil {
				usage.CacheReadTokens = lastUsage.PromptTokensDetails.CachedTokens
			}
			eventCh <- Event{Type: EventUsage, Usage: usage}
		}

		eventCh <- Event{Type: EventDone, FinishReason: finishReason}
	}()

	return eventCh, nil
}
