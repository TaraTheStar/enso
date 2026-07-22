// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TaraTheStar/azoth/llm"
)

// TestMessageFlags_RoundTrip pins the Synthetic/Ignored contract:
// both flags survive an AppendMessage → Load cycle. Without this,
// the compaction-summary detector would lose its anchor across a
// process restart and re-summarize the summary lossily.
func TestMessageFlags_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "flags.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "model-x", "prov-y", "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	id := w.SessionID()

	// A real user turn (both flags false).
	if _, err := w.AppendMessage(llm.Message{Role: "user", Content: "investigate the build"}, ""); err != nil {
		t.Fatal(err)
	}
	// A synthetic compaction-summary stand-in.
	if _, err := w.AppendMessage(llm.Message{
		Role:      "assistant",
		Content:   "[compacted summary of earlier conversation]\n\nGoals: X",
		Synthetic: true,
	}, ""); err != nil {
		t.Fatal(err)
	}
	// An ignored audit row that must NOT reach the model on next turn.
	if _, err := w.AppendMessage(llm.Message{
		Role:    "user",
		Content: "[operator note: skip this]",
		Ignored: true,
	}, ""); err != nil {
		t.Fatal(err)
	}

	state, err := Load(s, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 3 {
		t.Fatalf("history len: got %d, want 3", len(state.History))
	}
	if state.History[0].Synthetic || state.History[0].Ignored {
		t.Errorf("first message flags leaked: %+v", state.History[0])
	}
	if !state.History[1].Synthetic {
		t.Errorf("Synthetic flag lost in round trip on summary row")
	}
	if state.History[1].Ignored {
		t.Errorf("summary row should not be Ignored")
	}
	if !state.History[2].Ignored {
		t.Errorf("Ignored flag lost in round trip on audit row")
	}
}

// TestMessageReasoning_RoundTrip pins the replay-only reasoning
// contract: an assistant turn's chain-of-thought survives an
// AppendMessage → Load cycle so a resumed session can replay it.
func TestMessageReasoning_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "reasoning.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "model-x", "prov-y", "/cwd")
	if err != nil {
		t.Fatal(err)
	}
	id := w.SessionID()

	if _, err := w.AppendMessage(llm.Message{Role: "user", Content: "what is 2+2?"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendMessage(llm.Message{
		Role:      "assistant",
		Content:   "4",
		Reasoning: "The user is asking basic arithmetic. 2 + 2 = 4.",
	}, ""); err != nil {
		t.Fatal(err)
	}

	state, err := Load(s, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.History) != 2 {
		t.Fatalf("history len: got %d, want 2", len(state.History))
	}
	if state.History[0].Reasoning != "" {
		t.Errorf("user row should carry no reasoning, got %q", state.History[0].Reasoning)
	}
	if got := state.History[1].Reasoning; got != "The user is asking basic arithmetic. 2 + 2 = 4." {
		t.Errorf("assistant reasoning lost in round trip: got %q", got)
	}
}

// TestMessageReasoning_NotSentToProvider is the load-bearing invariant:
// reasoning is `json:"-"` and absent from MarshalJSON, so it never
// reaches a provider's wire payload (resending bloats context — the
// model re-derives its reasoning each turn).
func TestMessageReasoning_NotSentToProvider(t *testing.T) {
	m := llm.Message{Role: "assistant", Content: "4", Reasoning: "secret chain of thought"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret chain of thought") || strings.Contains(string(b), "reasoning") {
		t.Fatalf("reasoning leaked into provider wire payload: %s", b)
	}
	// Sanity: the content the model SHOULD see is still there.
	if !strings.Contains(string(b), `"4"`) {
		t.Fatalf("content missing from wire payload: %s", b)
	}
}

// TestFilterForRequest_DropsIgnored verifies the helper that every
// provider adapter calls right before serialization. The model must
// never see Ignored rows; Synthetic rows pass through unchanged.
func TestFilterForRequest_DropsIgnored(t *testing.T) {
	in := []llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "summary", Synthetic: true},
		{Role: "user", Content: "audit", Ignored: true},
		{Role: "user", Content: "actual question"},
	}
	got := llm.FilterForRequest(in)
	if len(got) != 3 {
		t.Fatalf("filtered len: got %d, want 3 (Ignored dropped)", len(got))
	}
	for _, m := range got {
		if m.Ignored {
			t.Errorf("Ignored row survived filter: %+v", m)
		}
	}
	if !got[1].Synthetic {
		t.Errorf("Synthetic stripped by filter (must pass through): %+v", got[1])
	}
}

// TestFilterForRequest_NoIgnoredReusesSlice avoids an allocation on
// the hot path: when no Ignored rows are present (the overwhelmingly
// common case), the same slice is returned.
func TestFilterForRequest_NoIgnoredReusesSlice(t *testing.T) {
	in := []llm.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
	}
	got := llm.FilterForRequest(in)
	if &got[0] != &in[0] {
		t.Errorf("filter allocated a new slice when no Ignored rows present")
	}
}
