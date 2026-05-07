// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, friendlyHTTPError(c.Endpoint, err)
	}

	if resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
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
