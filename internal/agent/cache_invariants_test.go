// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
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
	buildTurns(a) // appendToolMessage already ran pruneStaleToolMessages
	before := shapeOf(a)
	// Run it again explicitly — must be idempotent and shape-preserving.
	a.pruneStaleToolMessages()
	assertSameShape(t, before, shapeOf(a))

	// The system message must be index 0 and untouched.
	if a.History[0].Role != "system" || a.History[0].Content != "sys" {
		t.Fatalf("system message at index 0 was disturbed: %+v", a.History[0])
	}
}
