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
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/tools"
)

// The cache-boundary invariants (H2): pruning rewrites tool-result CONTENT
// but must never change the conversation's SHAPE — the system message, the
// message count, the per-message roles, and the tool_call_id pairing all
// stay fixed. Violating any of these either busts the prompt prefix cache
// upstream of where we intend (system message) or breaks OpenAI-shape
// request validity (orphaned tool_call_ids).

// snapshot captures the structural identity of History that pruning must
// preserve.
type historyShape struct {
	system      string
	roles       []string
	toolCallIDs []string
}

func shapeOf(a *Agent) historyShape {
	s := historyShape{}
	for _, m := range a.History {
		s.roles = append(s.roles, m.Role)
		if m.Role == "system" && s.system == "" {
			s.system = m.Content
		}
		if m.Role == "tool" {
			s.toolCallIDs = append(s.toolCallIDs, m.ToolCallID)
		}
	}
	return s
}

func assertSameShape(t *testing.T, before, after historyShape) {
	t.Helper()
	if before.system != after.system {
		t.Errorf("system message mutated by prune:\nbefore: %q\nafter:  %q", before.system, after.system)
	}
	if len(before.roles) != len(after.roles) {
		t.Fatalf("message count changed: before %d, after %d", len(before.roles), len(after.roles))
	}
	for i := range before.roles {
		if before.roles[i] != after.roles[i] {
			t.Errorf("role at %d changed: %q → %q", i, before.roles[i], after.roles[i])
		}
	}
	if len(before.toolCallIDs) != len(after.toolCallIDs) {
		t.Fatalf("tool_call_id count changed: before %d, after %d", len(before.toolCallIDs), len(after.toolCallIDs))
	}
	for i := range before.toolCallIDs {
		if before.toolCallIDs[i] != after.toolCallIDs[i] {
			t.Errorf("tool_call_id at %d changed: %q → %q", i, before.toolCallIDs[i], after.toolCallIDs[i])
		}
	}
}

func buildTurns(a *Agent) {
	for turn := 0; turn < 4; turn++ {
		a.addUser("do work")
		a.addAssistant("calling tools")
		a.addTool("bash", "call-bash-"+itoaA(turn), "lots of bash output\nmore\nlines", tools.ResultMeta{CacheKey: "bash:cmd" + itoaA(turn)})
		a.addTool("read", "call-read-"+itoaA(turn), "file contents here", tools.ResultMeta{PathsRead: []string{"/x/" + itoaA(turn) + ".go"}})
	}
}

func itoaA(n int) string { return string(rune('0' + n)) }

func TestCacheInvariant_ForcePrunePreservesShape(t *testing.T) {
	a := newPruneAgent(config.ContextPruneConfig{StaleAfter: 5})
	buildTurns(a)
	before := shapeOf(a)

	stubbed, _, _ := a.ForcePrune()
	if stubbed == 0 {
		t.Fatal("expected ForcePrune to stub at least one message")
	}
	assertSameShape(t, before, shapeOf(a))
}

func TestCacheInvariant_StaleStubbingPreservesShape(t *testing.T) {
	// Tight retention so the natural append-time stubbing fires.
	a := newPruneAgent(config.ContextPruneConfig{
		StaleAfter:    1,
		ToolRetention: map[string]int{"bash": 1, "read": 1},
	})
	buildTurns(a) // append-only: no stubbing happens during the appends
	before := shapeOf(a)
	// The reclaim pass stubs content but must preserve shape; running it
	// twice must be idempotent and shape-preserving.
	a.pruneStaleToolMessages()
	a.pruneStaleToolMessages()
	assertSameShape(t, before, shapeOf(a))

	// The system message must be index 0 and untouched.
	if a.History[0].Role != "system" || a.History[0].Content != "sys" {
		t.Fatalf("system message at index 0 was disturbed: %+v", a.History[0])
	}
}

// Compaction is the other half of H2 and the riskier one: unlike pruning
// (which only swaps a tool message's Content for a stub in place), it
// REBUILDS History wholesale. The cache-hot zone — the system prompt
// prefix that lives upstream of every conversation message — must come
// through byte-for-byte, or the very first request after a compaction
// re-pays the full system+tools cache write it had already amortised.
// The frozen zone must also stay append-only: the synthetic summary is
// inserted directly after the system prefix, never woven into it.
func TestCacheInvariant_CompactionPreservesSystemPrefix(t *testing.T) {
	mock := llmtest.NewT(t)
	// summariseHistory makes exactly one Chat call; hand it a canned summary.
	mock.Push(llmtest.Script{Text: "## Goal\nship the thing\n## Next Steps\nkeep going"})

	// Small window so recentTurnBudget pins only the freshest turn(s),
	// leaving older turns to actually compact. budget = window/4, floored
	// at 2000; large per-turn content keeps that to ~one recent turn.
	prov := &provider.Provider{
		Name:          "test",
		Client:        mock,
		Model:         "fake",
		ContextWindow: 8000,
		Pool:          llm.NewPool(1),
	}

	// Distinctive system prompt — this is the cache-hot prefix under test.
	sys := "SYSTEM PROMPT — tool defs + instructions — " + strings.Repeat("x", 64)
	hist := []llm.Message{{Role: "system", Content: sys}}
	big := strings.Repeat("alpha beta gamma delta ", 400) // ~9KB ≈ 2300 tokens/turn
	for i := 0; i < 6; i++ {
		n := itoaA(i)
		hist = append(hist,
			llm.Message{Role: "user", Content: "turn " + n + " " + big},
			llm.Message{Role: "assistant", Content: "reply " + n + " " + big},
		)
	}

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": prov},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		History:         hist,
		MaxTurns:        4,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	systemIdx, _, _, ok := compactionBoundary(a.History, a.recentTurnBudget())
	if !ok {
		t.Fatalf("test setup: expected a compactable boundary (recentTurnBudget=%d, %d msgs)",
			a.recentTurnBudget(), len(a.History))
	}
	prefixBefore := append([]llm.Message{}, a.History[:systemIdx+1]...)
	lenBefore := len(a.History)

	changed, err := a.forceCompact(context.Background(), "test")
	if err != nil {
		t.Fatalf("forceCompact: %v", err)
	}
	if !changed {
		t.Fatal("forceCompact reported no change; expected a compaction")
	}

	// 1. The cache-hot prefix is byte-identical, role and content.
	for i := range prefixBefore {
		if a.History[i].Role != prefixBefore[i].Role || a.History[i].Content != prefixBefore[i].Content {
			t.Errorf("cache-hot prefix mutated at index %d:\nbefore: %s / %q\nafter:  %s / %q",
				i, prefixBefore[i].Role, prefixBefore[i].Content, a.History[i].Role, a.History[i].Content)
		}
	}

	// 2. Append-only into the frozen zone: the synthetic summary sits
	//    immediately after the system prefix, not spliced inside it.
	synth := a.History[len(prefixBefore)]
	if !synth.Synthetic || synth.Role != "assistant" {
		t.Errorf("expected a synthetic assistant summary right after the system prefix, got role=%q synthetic=%v",
			synth.Role, synth.Synthetic)
	}

	// 3. The compaction actually shrank History (sanity: we tested the
	//    real path, not a no-op).
	if len(a.History) >= lenBefore {
		t.Errorf("compaction did not shrink history: before %d, after %d", lenBefore, len(a.History))
	}
}
