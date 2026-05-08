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
	EventDone
	EventError
)

// Event is a single item emitted by the streaming Chat loop.
type Event struct {
	Type      EventType
	Text      string
	ToolCalls []ToolCall
	Error     error
}

// ChatClient is the streaming-chat interface the agent and compaction
// loop use. *Client (the OpenAI-compatible HTTP client) is the
// production implementation; tests substitute a fake via the
// llmtest package.
type ChatClient interface {
	Chat(ctx context.Context, req ChatRequest) (<-chan Event, error)
}

// Client wraps one OpenAI-compatible HTTP endpoint.
type Client struct {
	Endpoint string
	APIKey   string
	Model    string

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

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// LLMConnState satisfies ConnStateReporter. Returning by value keeps the
// reader path lock-light — callers don't need to hold any reference to
// the tracker itself.
func (c *Client) LLMConnState() ConnState { return c.conn.get() }

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
func (c *Client) doChatRequest(ctx context.Context, body []byte) (*http.Response, error) {
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
func (c *Client) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	req.Stream = true
	req.Model = c.Model

	data, err := json.Marshal(req)
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
		go ParseSSE(resp.Body, sseDone)

		for raw := range sseDone {
			fmt.Fprintf(debugLog(), "sse: %s\n", string(raw))
			var chunk ChatCompletionChunk
			if err := json.Unmarshal(raw, &chunk); err != nil {
				eventCh <- Event{Type: EventError, Error: fmt.Errorf("unmarshal chunk: %w", err)}
				return
			}

			for _, ch := range chunk.Choices {
				delta := ch.Delta
				if delta.ReasoningContent != "" {
					eventCh <- Event{Type: EventReasoningDelta, Text: delta.ReasoningContent}
				}
				if delta.Content != "" {
					eventCh <- Event{Type: EventTextDelta, Text: delta.Content}
				}
				if len(delta.ToolCalls) > 0 {
					if err := acc.Merge(delta); err != nil {
						eventCh <- Event{Type: EventError, Error: fmt.Errorf("merge tool call: %w", err)}
						return
					}
				}
			}
		}

		if calls := acc.Finalize(); len(calls) > 0 {
			eventCh <- Event{Type: EventToolCallComplete, ToolCalls: calls}
		}

		eventCh <- Event{Type: EventDone}
	}()

	return eventCh, nil
}
