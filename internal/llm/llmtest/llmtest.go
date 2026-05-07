// SPDX-License-Identifier: AGPL-3.0-or-later

// Package llmtest provides a programmable llm.ChatClient for tests
// that need to drive the agent / workflow / compaction loops without
// standing up a real OpenAI-compatible server.
//
// Usage:
//
//	mock := llmtest.New()
//	mock.Push(llmtest.Script{
//	    ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"x"}`}}},
//	})
//	mock.Push(llmtest.Script{Text: "all done"})
//
//	provider := &llm.Provider{Client: mock, Pool: llm.NewPool(1), ContextWindow: 32_000}
//	// drive agent.Run with this provider...
//
// Each Chat() call consumes the next queued Script. Test
// fails (via t.Errorf hooked in NewT) if the queue runs dry while
// the production code still asks for more turns, or if any Scripts
// remain after the test ends.
package llmtest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// Script is one scheduled response. Events fire in order:
// optional ReasoningDelta, then TextDelta, then each ToolCall as a
// ToolCallComplete, then (optionally) the test blocks on Gate, then
// the channel closes — or, if Err is set, an Error event fires
// before close.
type Script struct {
	Reasoning string
	Text      string
	ToolCalls []llm.ToolCall
	// Gate, when non-nil, blocks the stream goroutine after emitting
	// the configured events but before closing the channel. Used for
	// tests that need to coordinate parallel turns (e.g. workflow
	// fan-out): the test holds the gate, lets the agent see the events,
	// then closes the gate to release the turn.
	Gate <-chan struct{}
	// Err, when non-nil, fires as an EventError just before close.
	Err error
}

// Mock is a programmable ChatClient. Construct with New or NewT.
type Mock struct {
	mu    sync.Mutex
	queue []Script
	calls []llm.ChatRequest
	t     *testing.T // optional; if set, errors are surfaced via t.Errorf
}

// New creates a bare Mock. Useful when the caller does its own
// assertion of leftover state.
func New() *Mock { return &Mock{} }

// NewT creates a Mock bound to a test. On Cleanup, any unconsumed
// scripts are reported as a test failure — the most common bug in
// scripted-mock tests is forgetting that the production code makes
// fewer turns than expected.
func NewT(t *testing.T) *Mock {
	t.Helper()
	m := &Mock{t: t}
	t.Cleanup(func() {
		m.mu.Lock()
		left := len(m.queue)
		m.mu.Unlock()
		if left > 0 {
			t.Errorf("llmtest: %d unconsumed scripts at end of test", left)
		}
	})
	return m
}

// Push appends one scheduled response.
func (m *Mock) Push(s Script) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, s)
}

// Calls returns a copy of every ChatRequest the mock has seen, in
// order. Useful for asserting that the production code sent the right
// messages or tools.
func (m *Mock) Calls() []llm.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]llm.ChatRequest, len(m.calls))
	copy(out, m.calls)
	return out
}

// CallCount is shorthand for len(Calls()).
func (m *Mock) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Chat satisfies llm.ChatClient. It pops the next script from the
// queue and emits its events on a fresh channel. If the queue is
// empty when called, that's a test bug — production code asked for
// more turns than the test prepared.
func (m *Mock) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.Event, error) {
	m.mu.Lock()
	m.calls = append(m.calls, req)
	if len(m.queue) == 0 {
		m.mu.Unlock()
		err := fmt.Errorf("llmtest: Chat called with empty script queue (call #%d)", len(m.calls))
		if m.t != nil {
			m.t.Helper()
			m.t.Errorf("%v", err)
		}
		return nil, err
	}
	s := m.queue[0]
	m.queue = m.queue[1:]
	m.mu.Unlock()

	ch := make(chan llm.Event, 8)
	go func() {
		defer close(ch)
		if s.Reasoning != "" {
			ch <- llm.Event{Type: llm.EventReasoningDelta, Text: s.Reasoning}
		}
		if s.Text != "" {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: s.Text}
		}
		for _, tc := range s.ToolCalls {
			ch <- llm.Event{Type: llm.EventToolCallComplete, ToolCalls: []llm.ToolCall{tc}}
		}
		if s.Gate != nil {
			select {
			case <-ctx.Done():
				return
			case <-s.Gate:
			}
		}
		if s.Err != nil {
			ch <- llm.Event{Type: llm.EventError, Error: s.Err}
			return
		}
		ch <- llm.Event{Type: llm.EventDone}
	}()
	return ch, nil
}

// compile-time assertion
var _ llm.ChatClient = (*Mock)(nil)
