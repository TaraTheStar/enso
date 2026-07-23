// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/azoth/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/tools"
)

// newCompactableAgent wires a prune-test agent with the provider/bus
// plumbing forceCompact needs (summariser provider pool, EventCompacted
// publish).
func newCompactableAgent(cfg config.ContextPruneConfig, mock *llmtest.Mock) *Agent {
	ag := newPruneAgent(cfg)
	prov := fakeProvider(mock)
	ag.Providers = map[string]*provider.Provider{"test": prov}
	ag.currentProvider = prov
	ag.Bus = bus.New()
	return ag
}

// findToolIdx returns the History index of the tool message with the
// given ToolCallID, or -1.
func findToolIdx(hist []llm.Message, id string) int {
	for i, m := range hist {
		if m.Role == "tool" && m.ToolCallID == id {
			return i
		}
	}
	return -1
}

// addBulkTurns appends n large user+assistant turns so the trailing-turn
// budget leaves the earlier turns in the compactable older block.
func addBulkTurns(ag *Agent, n int) {
	bulk := strings.Repeat("data ", 800) // ~1000 tokens per message
	for i := 0; i < n; i++ {
		ag.addUser(bulk)
		ag.addAssistant(bulk)
	}
}

// TestForceCompact_PinnedMetaSurvivesCompaction is the regression test
// for the pointer-keyed toolMeta bug: forceCompact used to key the
// preserved metadata on slice-element addresses taken while the slices
// were still growing, so a later append reallocated the backing array
// and the keys pointed into dead memory. Pinned tool messages then
// silently lost their toolMeta after the first compaction, and the NEXT
// compaction summarised the "no longer pinned" content away — defeating
// pinned_paths. Two pinned messages are required to trigger the
// reallocation; both must keep their meta across TWO compactions.
func TestForceCompact_PinnedMetaSurvivesCompaction(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "## Goal\nsummary one"})

	ag := newCompactableAgent(config.ContextPruneConfig{
		StaleAfter:  100,
		PinnedPaths: []string{"PLAN.md", "NOTES.md"},
	}, mock)

	ag.addUser("turn 1")
	ag.addTool("read", "plan", "THE PLAN CONTENT",
		tools.ResultMeta{PathsRead: []string{"/work/PLAN.md"}, CacheKey: "read:/work/PLAN.md"})
	ag.addTool("read", "notes", "THE NOTES CONTENT",
		tools.ResultMeta{PathsRead: []string{"/work/NOTES.md"}, CacheKey: "read:/work/NOTES.md"})
	addBulkTurns(ag, 12)

	assertPinned := func(stage, callID, wantContent string) {
		t.Helper()
		idx := findToolIdx(ag.History, callID)
		if idx < 0 {
			t.Fatalf("%s: pinned tool message %q vanished from History", stage, callID)
		}
		if !strings.Contains(ag.History[idx].Content, wantContent) {
			t.Errorf("%s: pinned %q content lost: %q", stage, callID, ag.History[idx].Content)
		}
		tm := ag.toolMeta[idx]
		if tm == nil {
			t.Fatalf("%s: pinned %q lost its toolMeta entry", stage, callID)
		}
		if !tm.pinned {
			t.Errorf("%s: pinned %q meta lost its pinned flag", stage, callID)
		}
	}

	changed, err := ag.forceCompact(context.Background(), "test")
	if err != nil {
		t.Fatalf("first forceCompact: %v", err)
	}
	if !changed {
		t.Fatal("first forceCompact reported no change; test setup must produce a compactable history")
	}
	assertPinned("after first compaction", "plan", "THE PLAN CONTENT")
	assertPinned("after first compaction", "notes", "THE NOTES CONTENT")

	// The pinned messages must still be exempt from pruning too — meta
	// loss would let ForcePrune stub them.
	ag.ForcePrune()
	assertPinned("after ForcePrune", "plan", "THE PLAN CONTENT")
	assertPinned("after ForcePrune", "notes", "THE NOTES CONTENT")

	// Second compaction: the pinned messages now sit in the older block
	// again (right after the system prompt). With their meta intact they
	// must survive verbatim a second time instead of being summarised.
	mock.Push(llmtest.Script{Text: "## Goal\nsummary two"})
	addBulkTurns(ag, 12)

	changed, err = ag.forceCompact(context.Background(), "test")
	if err != nil {
		t.Fatalf("second forceCompact: %v", err)
	}
	if !changed {
		t.Fatal("second forceCompact reported no change")
	}
	assertPinned("after second compaction", "plan", "THE PLAN CONTENT")
	assertPinned("after second compaction", "notes", "THE NOTES CONTENT")
}

// TestForceCompact_EmptySummaryAbortsWithoutRewriting covers the
// silent-history-loss bug: a summariser that streams no text deltas
// (e.g. a local reasoning model emitting only reasoning, or a turn cut
// off before any text) used to return ("", nil), and the rewrite then
// dropped every summarisable message with no summary in its place. The
// compaction must now surface an error and leave History untouched.
func TestForceCompact_EmptySummaryAbortsWithoutRewriting(t *testing.T) {
	mock := llmtest.NewT(t)
	// Reasoning only — no EventTextDelta ever fires.
	mock.Push(llmtest.Script{Reasoning: "deliberating at length without answering"})

	ag := newCompactableAgent(config.ContextPruneConfig{}, mock)
	addBulkTurns(ag, 12)
	before := append([]llm.Message(nil), ag.History...)

	changed, err := ag.forceCompact(context.Background(), "test")
	if err == nil {
		t.Fatal("forceCompact with an empty summary must return an error")
	}
	if changed {
		t.Error("forceCompact must report no change when the summary is empty")
	}
	if len(ag.History) != len(before) {
		t.Fatalf("History length changed on aborted compaction: %d → %d", len(before), len(ag.History))
	}
	for i := range before {
		if ag.History[i].Role != before[i].Role || ag.History[i].Content != before[i].Content {
			t.Errorf("History[%d] mutated on aborted compaction", i)
		}
	}
}

// TestForceCompact_TruncatedSummaryAborts: a summary cut off by the
// output cap (FinishLength) is unusable — its trailing sections (Next
// Steps, Critical Context…) are gone — so it must abort the rewrite
// just like an empty one.
func TestForceCompact_TruncatedSummaryAborts(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{
		Text:         "## Goal\npartial summary that got cut o",
		FinishReason: llm.FinishLength,
	})

	ag := newCompactableAgent(config.ContextPruneConfig{}, mock)
	addBulkTurns(ag, 12)
	lenBefore := len(ag.History)

	changed, err := ag.forceCompact(context.Background(), "test")
	if err == nil {
		t.Fatal("forceCompact with a truncated summary must return an error")
	}
	if changed {
		t.Error("forceCompact must report no change when the summary is truncated")
	}
	if len(ag.History) != lenBefore {
		t.Errorf("History rewritten despite truncated summary: %d → %d", lenBefore, len(ag.History))
	}
}
