// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestTranscripts_StoreAndGet(t *testing.T) {
	tr := NewTranscripts()
	hist := []llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	tr.Store("a1", hist)

	got := tr.Get("a1")
	if len(got) != 2 || got[0].Content != "hi" || got[1].Content != "hello" {
		t.Errorf("got %+v", got)
	}
}

func TestTranscripts_StoreCopiesSlice(t *testing.T) {
	tr := NewTranscripts()
	hist := []llm.Message{{Role: "user", Content: "original"}}
	tr.Store("a1", hist)

	// Mutate the source after Store; the registry copy must be unaffected.
	hist[0].Content = "MUTATED"
	got := tr.Get("a1")
	if got[0].Content != "original" {
		t.Errorf("registry not isolated: %q", got[0].Content)
	}
}

func TestTranscripts_GetMissingReturnsNil(t *testing.T) {
	tr := NewTranscripts()
	if got := tr.Get("nope"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestTranscripts_NilReceiverIsSafe(t *testing.T) {
	var tr *Transcripts
	tr.Store("a", []llm.Message{{Role: "user"}}) // must not panic
	if got := tr.Get("a"); got != nil {
		t.Errorf("nil receiver Get = %v, want nil", got)
	}
}

func TestTranscripts_EmptyIDIsNoOp(t *testing.T) {
	tr := NewTranscripts()
	tr.Store("", []llm.Message{{Role: "user", Content: "x"}})
	if got := tr.Get(""); got != nil {
		t.Errorf("empty-id Get = %v, want nil", got)
	}
}

func TestTranscripts_StoreOverwrites(t *testing.T) {
	tr := NewTranscripts()
	tr.Store("a1", []llm.Message{{Role: "user", Content: "first"}})
	tr.Store("a1", []llm.Message{{Role: "user", Content: "second"}})
	got := tr.Get("a1")
	if len(got) != 1 || got[0].Content != "second" {
		t.Errorf("got %+v, want [{user second}]", got)
	}
}
