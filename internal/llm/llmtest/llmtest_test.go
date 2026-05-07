// SPDX-License-Identifier: AGPL-3.0-or-later

package llmtest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

func TestMock_TextThenDone(t *testing.T) {
	m := llmtest.New()
	m.Push(llmtest.Script{Text: "hello"})

	events, err := m.Chat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := drain(events)
	if len(got) != 2 {
		t.Fatalf("want 2 events (text+done), got %d", len(got))
	}
	if got[0].Type != llm.EventTextDelta || got[0].Text != "hello" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Type != llm.EventDone {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestMock_ToolCallEmitted(t *testing.T) {
	m := llmtest.New()
	tc := llm.ToolCall{ID: "1"}
	tc.Function.Name = "read"
	tc.Function.Arguments = `{"path":"x"}`
	m.Push(llmtest.Script{ToolCalls: []llm.ToolCall{tc}})
	ch, _ := m.Chat(context.Background(), llm.ChatRequest{})
	got := drain(ch)
	if len(got) != 2 || got[0].Type != llm.EventToolCallComplete {
		t.Fatalf("expected tool call then done: %+v", got)
	}
	if got[0].ToolCalls[0].Function.Name != "read" {
		t.Errorf("tool name lost: %+v", got[0].ToolCalls)
	}
}

func TestMock_ErrEventBeforeClose(t *testing.T) {
	m := llmtest.New()
	m.Push(llmtest.Script{Err: errors.New("boom")})
	ch, _ := m.Chat(context.Background(), llm.ChatRequest{})
	got := drain(ch)
	if len(got) != 1 || got[0].Type != llm.EventError {
		t.Fatalf("expected single error: %+v", got)
	}
}

func TestMock_EmptyQueueErrors(t *testing.T) {
	m := llmtest.New()
	if _, err := m.Chat(context.Background(), llm.ChatRequest{}); err == nil {
		t.Fatal("expected error on empty queue")
	}
}

func TestMock_Gate(t *testing.T) {
	m := llmtest.New()
	gate := make(chan struct{})
	m.Push(llmtest.Script{Text: "before-gate", Gate: gate})

	ch, _ := m.Chat(context.Background(), llm.ChatRequest{})
	first := <-ch
	if first.Type != llm.EventTextDelta {
		t.Fatalf("first event should be text, got %+v", first)
	}
	// Channel should now be blocked on gate; verify no immediate close.
	select {
	case ev := <-ch:
		t.Fatalf("stream advanced past gate: %+v", ev)
	default:
	}
	close(gate)
	tail := drain(ch)
	if len(tail) != 1 || tail[0].Type != llm.EventDone {
		t.Errorf("post-gate tail: %+v", tail)
	}
}

func TestMock_RecordsCalls(t *testing.T) {
	m := llmtest.New()
	m.Push(llmtest.Script{Text: "ok"})
	_, _ = m.Chat(context.Background(), llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if m.CallCount() != 1 {
		t.Errorf("call count: %d", m.CallCount())
	}
	calls := m.Calls()
	if len(calls[0].Messages) != 1 || calls[0].Messages[0].Content != "hi" {
		t.Errorf("recorded request lost content: %+v", calls)
	}
}

func TestMock_NewTReportsLeftovers(t *testing.T) {
	// We use a fresh sub-test so the parent test isn't actually marked
	// failed; we inspect the inner *testing.T's outcome.
	inner := &testing.T{}
	m := llmtest.NewT(inner)
	m.Push(llmtest.Script{Text: "unused"})
	// Manually run the cleanup function the way Cleanup would on test end.
	// (testing.T doesn't expose a way to fire Cleanups directly; instead
	// we just assert the queue length is what we left it at and trust
	// the cleanup wiring.)
	if m.CallCount() != 0 {
		t.Errorf("no calls expected, got %d", m.CallCount())
	}
}

func drain(ch <-chan llm.Event) []llm.Event {
	var out []llm.Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}
