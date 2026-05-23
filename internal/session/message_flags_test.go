// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
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
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "investigate the build"}, ""); err != nil {
		t.Fatal(err)
	}
	// A synthetic compaction-summary stand-in.
	if err := w.AppendMessage(llm.Message{
		Role:      "assistant",
		Content:   "[compacted summary of earlier conversation]\n\nGoals: X",
		Synthetic: true,
	}, ""); err != nil {
		t.Fatal(err)
	}
	// An ignored audit row that must NOT reach the model on next turn.
	if err := w.AppendMessage(llm.Message{
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
