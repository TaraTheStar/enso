// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestCompactPreview_NothingToDoOnTinyHistory(t *testing.T) {
	a := &Agent{History: []llm.Message{
		{Role: "system", Content: "you are an agent"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}}
	got := a.CompactPreview()
	if !got.NothingToDo {
		t.Errorf("tiny history should report NothingToDo, got %+v", got)
	}
}

func TestCompactPreview_ReportsCountsWithoutMutating(t *testing.T) {
	// Build a history big enough that the boundary detector finds
	// older turns to summarise. recentTurnsToPin is the number of
	// trailing turns kept verbatim; we need something old enough to
	// be eligible.
	bulk := strings.Repeat("padding text bytes ", 200) // ~3.6k chars per turn
	hist := []llm.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 30; i++ {
		hist = append(hist,
			llm.Message{Role: "user", Content: bulk},
			llm.Message{Role: "assistant", Content: bulk},
		)
	}
	original := append([]llm.Message(nil), hist...)

	a := &Agent{History: hist}
	got := a.CompactPreview()
	if got.NothingToDo {
		t.Fatalf("expected work to do for %d-message history", len(hist))
	}
	if got.MessagesToSummarise <= 0 {
		t.Errorf("MessagesToSummarise=%d, want >0", got.MessagesToSummarise)
	}
	if got.BeforeTokens <= 0 {
		t.Errorf("BeforeTokens=%d, want >0", got.BeforeTokens)
	}
	if got.EstAfterTokens >= got.BeforeTokens {
		t.Errorf("EstAfterTokens=%d should be less than BeforeTokens=%d",
			got.EstAfterTokens, got.BeforeTokens)
	}

	// Mutation guard: the preview must not touch a.History.
	if len(a.History) != len(original) {
		t.Errorf("history length changed: %d → %d", len(original), len(a.History))
	}
	for i := range original {
		if a.History[i].Content != original[i].Content {
			t.Errorf("history[%d] content changed", i)
		}
	}
}

func TestCompactPreview_EmptyHistory(t *testing.T) {
	a := &Agent{History: nil}
	got := a.CompactPreview()
	if !got.NothingToDo {
		t.Errorf("empty history should be NothingToDo, got %+v", got)
	}
}
