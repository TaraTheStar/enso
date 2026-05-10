// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/tools"
)

// newPruneAgent constructs a minimal Agent suitable for prune-logic
// tests — no providers, no bus, no writer. Pruning only touches
// History and toolMeta; the other fields are nil-tolerated.
func newPruneAgent(cfg config.ContextPruneConfig) *Agent {
	resolved := cfg.Resolve()
	return &Agent{
		History:  []llm.Message{{Role: "system", Content: "sys"}},
		toolMeta: map[int]*toolMessageMeta{},
		pruneCfg: resolved,
	}
}

// addUser bumps the user-turn counter and appends a user message.
// Mirrors what appendUserMessage does in the production path so
// tests exercise the same accounting.
func (a *Agent) addUser(content string) {
	a.userTurnCounter++
	a.History = append(a.History, llm.Message{Role: "user", Content: content})
}

// addAssistant appends a bare assistant message.
func (a *Agent) addAssistant(content string) {
	a.History = append(a.History, llm.Message{Role: "assistant", Content: content})
}

// addTool runs through appendToolMessage so tests cover the actual
// production code path (dedup + invalidation + stubbing).
func (a *Agent) addTool(name, callID, content string, meta tools.ResultMeta) {
	a.appendToolMessage(llm.Message{
		Role:       "tool",
		Name:       name,
		ToolCallID: callID,
		Content:    content,
	}, meta)
}

// A1 / A2: tool messages older than the per-tool retention threshold
// get stubbed; messages within the threshold stay verbatim.
func TestPrune_StaleStubbingByPerToolThreshold(t *testing.T) {
	ag := newPruneAgent(config.ContextPruneConfig{
		StaleAfter: 5,
		ToolRetention: map[string]int{
			"bash": 2, // bash retained for 2 user-turns
			"read": 5, // read retained for 5
		},
	})

	// turn 1: read big, bash quick
	ag.addUser("turn 1")
	ag.addTool("read", "r1", strings.Repeat("read1\n", 50), tools.ResultMeta{
		PathsRead: []string{"/abs/file.go"},
		CacheKey:  "read:/abs/file.go:1-50",
	})
	ag.addTool("bash", "b1", strings.Repeat("bashout\n", 50), tools.ResultMeta{
		CacheKey: "bash:ls",
	})

	// turn 2 — bash from turn 1 is now 1 user-turn old, still verbatim.
	ag.addUser("turn 2")
	ag.pruneStaleToolMessages()
	if isStub(findToolByID(ag.History, "b1").Content) {
		t.Errorf("turn 2: bash from turn 1 should not yet be stubbed (1 turn old, threshold=2)")
	}

	// turn 3 — bash is 2 user-turns old; threshold is 2; > threshold? no (2 == threshold, not >). Still verbatim.
	ag.addUser("turn 3")
	ag.pruneStaleToolMessages()
	if isStub(findToolByID(ag.History, "b1").Content) {
		t.Errorf("turn 3: bash 2 turns old, threshold=2, should still be verbatim")
	}

	// turn 4 — bash is 3 user-turns old; > 2 threshold; stub.
	ag.addUser("turn 4")
	ag.pruneStaleToolMessages()
	if !isStub(findToolByID(ag.History, "b1").Content) {
		t.Errorf("turn 4: bash 3 turns old, threshold=2, should be stubbed: %q", findToolByID(ag.History, "b1").Content)
	}

	// read on turn 1 is 3 turns old, threshold=5, still verbatim.
	if isStub(findToolByID(ag.History, "r1").Content) {
		t.Errorf("turn 4: read 3 turns old, threshold=5, should be verbatim")
	}
}

// A3: dedup by cache key — second tool result with same key stubs
// the first, regardless of retention threshold.
func TestPrune_DedupByCacheKey(t *testing.T) {
	ag := newPruneAgent(config.ContextPruneConfig{StaleAfter: 100}) // disable stale-stubbing for this test

	ag.addUser("u1")
	ag.addTool("read", "r1", "first read content",
		tools.ResultMeta{PathsRead: []string{"/abs/x.go"}, CacheKey: "read:/abs/x.go:1-100"})

	ag.addUser("u2")
	ag.addTool("read", "r2", "second read content (same key)",
		tools.ResultMeta{PathsRead: []string{"/abs/x.go"}, CacheKey: "read:/abs/x.go:1-100"})

	first := findToolByID(ag.History, "r1")
	if !isStub(first.Content) {
		t.Errorf("first read should have been stubbed by dedup: %q", first.Content)
	}
	second := findToolByID(ag.History, "r2")
	if isStub(second.Content) {
		t.Errorf("second read should be verbatim: %q", second.Content)
	}
}

// A4: a write/edit that reports PathsWritten triggers stubbing of
// any earlier read of the same path.
func TestPrune_PostEditInvalidatesReads(t *testing.T) {
	ag := newPruneAgent(config.ContextPruneConfig{StaleAfter: 100}) // disable stale-stubbing

	ag.addUser("u1")
	ag.addTool("read", "r1", "pre-edit content",
		tools.ResultMeta{PathsRead: []string{"/abs/foo.go"}, CacheKey: "read:/abs/foo.go:1-50"})

	ag.addUser("u2")
	ag.addTool("edit", "e1", "edited foo.go (1 replacement)",
		tools.ResultMeta{PathsWritten: []string{"/abs/foo.go"}, CacheKey: "edit:/abs/foo.go"})

	r := findToolByID(ag.History, "r1")
	if !isStub(r.Content) {
		t.Errorf("read of foo.go should have been stubbed after edit: %q", r.Content)
	}
}

// C1: a read of a pinned path is not stubbed even when stale, and
// is preserved by ForcePrune.
func TestPrune_PinnedReadSurvives(t *testing.T) {
	ag := newPruneAgent(config.ContextPruneConfig{
		StaleAfter:  1,
		PinnedPaths: []string{"PLAN.md"},
	})

	ag.addUser("u1")
	ag.addTool("read", "plan", "the plan content...",
		tools.ResultMeta{PathsRead: []string{"/work/PLAN.md"}, CacheKey: "read:/work/PLAN.md:1-200"})
	ag.addTool("read", "other", "some other file",
		tools.ResultMeta{PathsRead: []string{"/work/other.go"}, CacheKey: "read:/work/other.go:1-50"})

	// Push 5 user turns to make turn-1 reads thoroughly stale.
	for i := 0; i < 5; i++ {
		ag.addUser("u")
	}
	ag.pruneStaleToolMessages()

	if isStub(findToolByID(ag.History, "plan").Content) {
		t.Errorf("pinned PLAN.md read should not be stubbed")
	}
	if !isStub(findToolByID(ag.History, "other").Content) {
		t.Errorf("non-pinned read should be stubbed")
	}

	// ForcePrune is even more aggressive — pinned still survives.
	ag.ForcePrune()
	if isStub(findToolByID(ag.History, "plan").Content) {
		t.Errorf("ForcePrune should not stub pinned content")
	}
}

// pathMatchesPinned: suffix match aligned to a path separator.
func TestPathMatchesPinned(t *testing.T) {
	cases := []struct {
		abs    string
		pinned []string
		want   bool
	}{
		{"/home/x/PLAN.md", []string{"PLAN.md"}, true},
		{"/work/PLAN.md", []string{"PLAN.md"}, true},                 // sandbox path
		{"/home/x/SUBPLAN.md", []string{"PLAN.md"}, false},           // suffix without separator boundary
		{"/home/x/docs/spec.md", []string{"docs/spec.md"}, true},     // multi-segment suffix at separator boundary
		{"/home/x/notdocs/spec.md", []string{"docs/spec.md"}, false}, // suffix without separator boundary — `t` before `docs`
		{"PLAN.md", []string{"PLAN.md"}, true},                       // bare filename match
		{"/abs/file.go", []string{"PLAN.md"}, false},
	}
	for _, c := range cases {
		got := pathMatchesPinned(c.abs, c.pinned)
		if got != c.want {
			t.Errorf("pathMatchesPinned(%q, %v) = %v, want %v", c.abs, c.pinned, got, c.want)
		}
	}
}

// ForcePrune: stubs everything except messages from the most recent
// user-turn; pinned messages survive.
func TestPrune_ForcePruneAggressive(t *testing.T) {
	ag := newPruneAgent(config.ContextPruneConfig{StaleAfter: 100, PinnedPaths: []string{"PLAN.md"}})

	ag.addUser("u1")
	ag.addTool("bash", "b1", strings.Repeat("OUT\n", 100), tools.ResultMeta{CacheKey: "bash:foo"})
	ag.addTool("read", "plan", "plan content",
		tools.ResultMeta{PathsRead: []string{"/work/PLAN.md"}, CacheKey: "read:/work/PLAN.md:1-50"})

	ag.addUser("u2")
	ag.addTool("read", "r-recent", "current file content",
		tools.ResultMeta{PathsRead: []string{"/abs/recent.go"}, CacheKey: "read:/abs/recent.go:1-20"})

	stubbed, before, after := ag.ForcePrune()
	if stubbed != 1 {
		t.Errorf("ForcePrune: stubbed %d, want 1 (only old bash; plan pinned, recent kept)", stubbed)
	}
	if after >= before {
		t.Errorf("ForcePrune: tokens did not decrease (%d → %d)", before, after)
	}
	if !isStub(findToolByID(ag.History, "b1").Content) {
		t.Errorf("old bash should be stubbed")
	}
	if isStub(findToolByID(ag.History, "plan").Content) {
		t.Errorf("pinned plan should survive")
	}
	if isStub(findToolByID(ag.History, "r-recent").Content) {
		t.Errorf("most recent read should survive")
	}
}

// PrefixBreakdown: classifies messages into the right buckets.
func TestPrefixBreakdown(t *testing.T) {
	ag := newPruneAgent(config.ContextPruneConfig{StaleAfter: 100})
	ag.addUser("hello")
	ag.addAssistant("hi")
	ag.addTool("read", "r1", "active read content",
		tools.ResultMeta{PathsRead: []string{"/x.go"}, CacheKey: "read:/x.go:1-10"})
	ag.addTool("read", "rp", "pinned content",
		tools.ResultMeta{PathsRead: []string{"/PLAN.md"}, CacheKey: "read:/PLAN.md:1-5"})
	// Simulate pin-flag for the rp message:
	for idx, m := range ag.History {
		if m.Role == "tool" && m.ToolCallID == "rp" {
			ag.toolMeta[idx].pinned = true
		}
	}

	bd := ag.PrefixBreakdown()
	if bd.System == 0 {
		t.Errorf("expected non-zero system tokens")
	}
	if bd.Conversation == 0 {
		t.Errorf("expected non-zero conversation tokens")
	}
	if bd.Pinned == 0 {
		t.Errorf("expected non-zero pinned tokens")
	}
	if bd.ToolActive == 0 {
		t.Errorf("expected non-zero tool-active tokens")
	}
	if bd.Total != bd.System+bd.Conversation+bd.Pinned+bd.ToolActive+bd.ToolStubbed {
		t.Errorf("Total %d does not equal sum of categories (sys=%d conv=%d pin=%d active=%d stubbed=%d)",
			bd.Total, bd.System, bd.Conversation, bd.Pinned, bd.ToolActive, bd.ToolStubbed)
	}
}

// findToolByID is a test helper — returns the message in `hist`
// whose ToolCallID matches `id`, or a zero Message if absent.
func findToolByID(hist []llm.Message, id string) llm.Message {
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == id {
			return m
		}
	}
	return llm.Message{}
}
