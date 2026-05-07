// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func mkAsstWithCalls(content string, ids ...string) llm.Message {
	calls := make([]llm.ToolCall, 0, len(ids))
	for _, id := range ids {
		tc := llm.ToolCall{ID: id, Type: "function"}
		tc.Function.Name = "read"
		calls = append(calls, tc)
	}
	return llm.Message{Role: "assistant", Content: content, ToolCalls: calls}
}

func mkTool(callID, content string) llm.Message {
	return llm.Message{Role: "tool", Name: "read", ToolCallID: callID, Content: content}
}

func TestBackfillInterrupted_NoToolCallsClean(t *testing.T) {
	hist := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	patched, interrupted := backfillInterrupted(hist)
	if interrupted {
		t.Errorf("interrupted=true on clean history")
	}
	if len(patched) != len(hist) {
		t.Errorf("patched len changed: got %d want %d", len(patched), len(hist))
	}
}

func TestBackfillInterrupted_AllReplied(t *testing.T) {
	hist := []llm.Message{
		{Role: "user", Content: "hi"},
		mkAsstWithCalls("", "A", "B"),
		mkTool("A", "ra"),
		mkTool("B", "rb"),
		{Role: "user", Content: "thanks"},
	}
	patched, interrupted := backfillInterrupted(hist)
	if interrupted {
		t.Errorf("all replies present: want interrupted=false")
	}
	if len(patched) != len(hist) {
		t.Errorf("patched len changed unexpectedly: got %d want %d", len(patched), len(hist))
	}
}

func TestBackfillInterrupted_OneMissingReply(t *testing.T) {
	hist := []llm.Message{
		{Role: "user", Content: "hi"},
		mkAsstWithCalls("", "A", "B"),
		mkTool("A", "ra"),
		// "B" reply missing — process died after tool A returned.
	}
	patched, interrupted := backfillInterrupted(hist)
	if !interrupted {
		t.Errorf("want interrupted=true")
	}
	// Synthetic message appended at the end.
	if len(patched) != len(hist)+1 {
		t.Fatalf("patched len = %d, want %d", len(patched), len(hist)+1)
	}
	last := patched[len(patched)-1]
	if last.Role != "tool" || last.ToolCallID != "B" {
		t.Errorf("last patched message = %+v, want tool reply for B", last)
	}
	if last.Content == "" {
		t.Errorf("synthetic content should describe the interruption")
	}
}

func TestBackfillInterrupted_MultipleMissingReplies(t *testing.T) {
	hist := []llm.Message{
		mkAsstWithCalls("", "A", "B", "C"),
		// no replies at all
	}
	patched, interrupted := backfillInterrupted(hist)
	if !interrupted {
		t.Errorf("want interrupted=true")
	}
	if len(patched) != 4 { // original 1 + 3 synthetic
		t.Fatalf("patched len = %d, want 4", len(patched))
	}
	got := map[string]bool{}
	for _, m := range patched[1:] {
		if m.Role != "tool" {
			t.Errorf("patched[%d] role = %q, want tool", 1, m.Role)
		}
		got[m.ToolCallID] = true
	}
	for _, want := range []string{"A", "B", "C"} {
		if !got[want] {
			t.Errorf("missing synthetic reply for %q", want)
		}
	}
}

func TestBackfillInterrupted_OnlyAnswerOncePerID(t *testing.T) {
	// If the same call id somehow appears twice (shouldn't happen, but be
	// defensive), only one synthetic should be appended.
	hist := []llm.Message{
		mkAsstWithCalls("", "A"),
		mkAsstWithCalls("", "A"),
	}
	patched, interrupted := backfillInterrupted(hist)
	if !interrupted {
		t.Errorf("want interrupted=true")
	}
	syntheticCount := 0
	for _, m := range patched {
		if m.Role == "tool" && m.ToolCallID == "A" {
			syntheticCount++
		}
	}
	if syntheticCount != 1 {
		t.Errorf("synthetic count for A = %d, want 1", syntheticCount)
	}
}
