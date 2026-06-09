// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// Tests for structured prompt headers, update-mode detection,
// and token-budget tail sizing.

// TestBuildSummariseRequest_StructuredHeaders pins the seven-section
// contract on the fresh-summary prompt. A drift in the system prompt
// shape that drops or renames a header would degrade resume coherence;
// catch it at the prompt-render level rather than waiting for an
// LLM-output regression.
func TestBuildSummariseRequest_StructuredHeaders(t *testing.T) {
	sys, _, _ := buildSummariseRequest([]llm.Message{u("hi"), a("hello")}, "")
	want := []string{
		"## Goal",
		"## Constraints & Preferences",
		"## Progress",
		"### Done",
		"### In Progress",
		"### Blocked",
		"## Key Decisions",
		"## Next Steps",
		"## Critical Context",
		"## Relevant Files",
	}
	for _, h := range want {
		if !strings.Contains(sys, h) {
			t.Errorf("prompt missing section header %q", h)
		}
	}
}

// TestBuildSummariseRequest_UpdateModeFiresOnSynthetic verifies the
// prompt mode switches when a prior Synthetic compaction summary
// leads `older`. The mode-specific template introduces the "## PRIOR
// SUMMARY" / "## NEW EVENTS" structure absent from the fresh prompt.
func TestBuildSummariseRequest_UpdateModeFiresOnSynthetic(t *testing.T) {
	prior := llm.Message{
		Role:      "assistant",
		Content:   "[compacted summary of earlier conversation]\n\n## Goal\nresearch tokens",
		Synthetic: true,
	}
	older := []llm.Message{prior, u("now also check caching"), a("ok")}

	sys, user, _ := buildSummariseRequest(older, "")

	if !strings.Contains(sys, "UPDATING an existing structured summary") {
		t.Errorf("system prompt did not switch to update mode:\n%s", sys)
	}
	if !strings.Contains(user, "## PRIOR SUMMARY") {
		t.Errorf("user payload missing PRIOR SUMMARY header:\n%s", user)
	}
	if !strings.Contains(user, "## NEW EVENTS") {
		t.Errorf("user payload missing NEW EVENTS header:\n%s", user)
	}
	// The prior summary's body must be inlined (without the bracket
	// envelope).
	if !strings.Contains(user, "## Goal\nresearch tokens") {
		t.Errorf("prior summary body not inlined under PRIOR SUMMARY:\n%s", user)
	}
	if strings.Contains(user, "[compacted summary of earlier conversation]") {
		t.Errorf("bracket envelope leaked into update payload:\n%s", user)
	}
}

// TestBuildSummariseRequest_FreshModeWithoutSynthetic confirms a
// vanilla history (no prior summary) uses the fresh template — no
// PRIOR SUMMARY block, regular ## Goal-driven structure.
func TestBuildSummariseRequest_FreshModeWithoutSynthetic(t *testing.T) {
	older := []llm.Message{u("start"), a("ok")}
	sys, user, _ := buildSummariseRequest(older, "")

	if strings.Contains(sys, "UPDATING") {
		t.Errorf("fresh history should not trigger update mode:\n%s", sys)
	}
	if strings.Contains(user, "## PRIOR SUMMARY") {
		t.Errorf("PRIOR SUMMARY block must not appear without a prior summary:\n%s", user)
	}
}

// TestBuildSummariseRequest_LegacyPrefixFallback covers session DBs
// that pre-date the Synthetic flag: a summary row appended before the
// flag landed wouldn't have it, but its content prefix is the same.
// The leading-position bracket-prefix fallback should still detect it.
func TestBuildSummariseRequest_LegacyPrefixFallback(t *testing.T) {
	priorNoFlag := llm.Message{
		Role:    "assistant",
		Content: "[compacted summary of earlier conversation]\n\n## Goal\nold session",
		// Synthetic NOT set — legacy row.
	}
	older := []llm.Message{priorNoFlag, u("next"), a("ok")}
	sys, _, _ := buildSummariseRequest(older, "")
	if !strings.Contains(sys, "UPDATING") {
		t.Errorf("legacy bracket-prefix prior summary not detected:\n%s", sys)
	}
}

// TestTailTurnsForBudget_RespectsBudget walks a known-size history and
// confirms the budget-bounded turn count matches a manual calculation.
// Each turn is ~6 chars → ~1.5 tokens via the 4-char heuristic; with
// budget=3 we should fit two turns ("u" + "a" each tiny) and reject
// the third for prudence.
func TestTailTurnsForBudget_RespectsBudget(t *testing.T) {
	// 5 turns of small messages. llm.Estimate("hi")=1 (4-char rule).
	hist := []llm.Message{}
	for i := 0; i < 5; i++ {
		hist = append(hist, u("hi"), a("hi"))
	}

	// Large budget → all turns fit.
	if n := tailTurnsForBudget(hist, 1_000_000); n != 5 {
		t.Errorf("huge budget: got %d turns, want 5", n)
	}
	// Zero budget → falls back to 1 (never pin zero).
	if n := tailTurnsForBudget(hist, 0); n != 1 {
		t.Errorf("zero budget: got %d turns, want 1 (minimum pin)", n)
	}
}

// TestSummaryTemplatesRetireCompletedWork pins the "retire when done"
// guidance in both summary templates. Without it, a solved goal lingers
// under ## Goal / ## Next Steps and the model redoes finished work — the
// compaction-loop regression this guards.
func TestSummaryTemplatesRetireCompletedWork(t *testing.T) {
	// Fresh template (no prior summary).
	freshSys, _, _ := buildSummariseRequest([]llm.Message{u("hi"), a("done")}, "")
	if !strings.Contains(freshSys, "awaiting next user instruction") {
		t.Errorf("fresh ## Goal lacks completed-goal retirement guidance:\n%s", freshSys)
	}
	if !strings.Contains(freshSys, "None — awaiting user instruction") {
		t.Errorf("fresh ## Next Steps lacks empty-when-done guidance:\n%s", freshSys)
	}

	// Update template (leading Synthetic prior summary).
	older := []llm.Message{
		{Role: "assistant", Synthetic: true, Content: "[compacted summary of earlier conversation]\n\n## Goal\nfix X"},
		u("now do Y"), a("did Y"),
	}
	updateSys, _, _ := buildSummariseRequest(older, "")
	if !strings.Contains(updateSys, "Retire a Goal or Next Step") {
		t.Errorf("update template missing retire-when-done rule:\n%s", updateSys)
	}
}

// TestCompletionAnchor covers the seed derived for seedless (auto/overflow)
// compactions: a completed exchange yields an anchor; anything mid-work
// yields "" so we never falsely assert completion.
func TestCompletionAnchor(t *testing.T) {
	const wantSub = "finished work, not an open task"

	cases := []struct {
		name        string
		block       []llm.Message
		wantNonZero bool
	}{
		{"completed answer", []llm.Message{u("fix X"), aWithCalls("", "c1"), toolReply("c1", "ok"), a("fixed X")}, true},
		{"trailing tool result", []llm.Message{u("fix X"), aWithCalls("", "c1"), toolReply("c1", "ok")}, false},
		{"dangling assistant tool-call", []llm.Message{u("fix X"), aWithCalls("working", "c1")}, false},
		{"unanswered user turn", []llm.Message{a("done"), u("fix X")}, false},
		{"empty assistant answer", []llm.Message{u("fix X"), a("   ")}, false},
		{"empty block", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := completionAnchor(tc.block)
			if tc.wantNonZero {
				if !strings.Contains(got, wantSub) {
					t.Errorf("want anchor containing %q, got %q", wantSub, got)
				}
			} else if got != "" {
				t.Errorf("want empty anchor for mid-work block, got %q", got)
			}
		})
	}
}

// TestBuildSummariseRequest_SeedRendered confirms a non-empty seed (e.g.
// the completion anchor) reaches the summariser's user payload so the
// recap is framed around the finished boundary.
func TestBuildSummariseRequest_SeedRendered(t *testing.T) {
	_, user, _ := buildSummariseRequest([]llm.Message{u("hi"), a("done")},
		"the preceding user request has been completed and answered")
	if !strings.Contains(user, "the preceding user request has been completed and answered") {
		t.Errorf("seed anchor not rendered into summariser payload:\n%s", user)
	}
}

// TestTailTurnsForBudget_LargeContentFewTurns verifies the budget
// pulls back the turn count when individual messages are large. This
// is the key win over the fixed `recentTurnsToPin = 6`: a session that
// just dumped a 20 KB grep result should pin fewer turns so the
// compacted prefix has room to grow.
func TestTailTurnsForBudget_LargeContentFewTurns(t *testing.T) {
	huge := strings.Repeat("x ", 10_000) // ~5000 tokens by 4-char heuristic
	hist := []llm.Message{
		u("turn1"), a("ok"),
		u("turn2"), a("ok"),
		u("turn3"), a(huge), // this one is enormous
		u("turn4"), a("ok"),
	}
	// Budget = 2000 tokens → can't fit the huge turn; should bound to
	// the turns AFTER it.
	turns := tailTurnsForBudget(hist, 2000)
	if turns >= 4 {
		t.Errorf("budget=2000: got %d turns, want < 4 (the huge turn must NOT fit)", turns)
	}
	if turns < 1 {
		t.Errorf("budget=2000: got %d turns, want >= 1", turns)
	}
}
