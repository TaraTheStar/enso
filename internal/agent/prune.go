// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"fmt"
	"strings"

	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/tools"
)

// toolMessageMeta is the per-tool-message bookkeeping the prune
// subsystem (A1–A4, C1) maintains alongside Agent.History. Indexed by
// the message's slot in History; rebuildToolMeta() rewrites the index
// after compaction shifts slots.
type toolMessageMeta struct {
	toolName     string
	cacheKey     string
	pathsRead    []string
	pathsWritten []string
	// userTurnAt is the value of Agent.userTurnCounter at the moment
	// this tool message was appended. The "age" of a tool message is
	// `Agent.userTurnCounter - userTurnAt`; messages older than the
	// configured staleAfter become stubs.
	userTurnAt int
	// stubbed is true once Content has been replaced with a short
	// placeholder. Idempotent — re-stubbing a stub is a no-op.
	stubbed bool
	// pinned messages are exempt from stubbing AND from compaction's
	// older-block selection. C1: a `read` whose PathsRead intersects
	// pruneCfg.PinnedPaths sets this on append.
	pinned bool
}

// stubMarkerPrefix is the leading text the stubber writes into a
// tool message's Content. It's used by isStub() to detect already-
// stubbed messages so re-stubbing is idempotent and so compaction's
// summariser doesn't re-summarise placeholders.
const stubMarkerPrefix = "[pruned tool output: "

// isStub reports whether `s` looks like the stub-replaced content.
// Used by compaction-time renderers to skip already-stubbed entries.
func isStub(s string) bool {
	return strings.HasPrefix(s, stubMarkerPrefix)
}

// stubFor builds the placeholder content that replaces a tool
// message's payload. Keeps the message's footprint to ~50 bytes
// while preserving enough information for the model to know that a
// tool ran and roughly what it did.
func stubFor(toolName, original string) string {
	lines := strings.Count(original, "\n") + 1
	if original == "" {
		lines = 0
	}
	return fmt.Sprintf("%s%s, %d line%s, payload dropped to free context]",
		stubMarkerPrefix, toolName, lines, plural1(lines))
}

func plural1(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// pathMatchesPinned tests whether `abs` should be considered a pinned
// path under suffix-match semantics. A pinned-paths config of
// ["PLAN.md"] matches "/home/.../PLAN.md" *and* "/work/PLAN.md"
// (sandbox-mode), which is the rationale for suffix matching: paths
// differ inside vs. outside containers, but the filename is stable.
//
// To avoid spurious matches ("MAIN.md" matching "PLAN.md") the suffix
// must align on a path separator unless the pinned entry already
// starts with one.
func pathMatchesPinned(abs string, pinned []string) bool {
	for _, p := range pinned {
		if p == "" {
			continue
		}
		if !strings.HasSuffix(abs, p) {
			continue
		}
		if len(abs) == len(p) {
			return true
		}
		boundary := abs[len(abs)-len(p)-1]
		if boundary == '/' || boundary == '\\' {
			return true
		}
	}
	return false
}

// inCodeDefaultRetention is the per-tool retention used when neither
// the user nor a global StaleAfter is configured. Tuned for typical
// agentic-coding workloads:
//   - read longest (file content is referenced more than once)
//   - bash short (command output ages fast)
//   - grep/glob shorter (consumed once, then specific files get read)
//   - edit/write minimal (only the diff confirmation matters)
var inCodeDefaultRetention = map[string]int{
	"read":  8,
	"bash":  3,
	"grep":  2,
	"glob":  2,
	"edit":  1,
	"write": 1,
}

// staleAfterFor returns how many user-turns of retention a given tool
// gets before its results become eligible for stubbing.
//
// Lookup precedence:
//  1. user-explicit per-tool override (pruneCfg.ToolRetention)
//  2. user-explicit global StaleAfter (when > 0)
//  3. in-code per-tool default
//  4. fallback (5)
//
// Step 2 deliberately shadows step 3 so a user-set
// `stale_after = 1` actually flips every tool, not just the unlisted
// ones.
func (a *Agent) staleAfterFor(toolName string) int {
	if v, ok := a.pruneCfg.ToolRetention[toolName]; ok && v > 0 {
		return v
	}
	if a.pruneCfg.StaleAfter > 0 {
		return a.pruneCfg.StaleAfter
	}
	if v, ok := inCodeDefaultRetention[toolName]; ok {
		return v
	}
	return 5
}

// appendToolMessage is the prune-aware variant of appendMessage that
// the agent's tool-execution path uses. It:
//
//  1. records sidecar metadata for the new message,
//  2. runs A3 (same cacheKey → stub the older),
//  3. runs A4 (paths written → stub matching prior reads),
//  4. delegates to appendMessage to persist + grow History,
//  5. runs A1/A2 (pruneStaleToolMessages) — stubs tool messages older
//     than their per-tool retention threshold.
//
// Steps 2–3 happen *before* appending so the new message's index is
// correct. The new message itself is never stubbed by these steps —
// its content is the freshest signal and stays verbatim.
func (a *Agent) appendToolMessage(msg llm.Message, meta tools.ResultMeta) {
	// Late-init for backwards-compat — older callers (or tests
	// constructing an Agent without going through New) may not have
	// the map allocated yet.
	if a.toolMeta == nil {
		a.toolMeta = map[int]*toolMessageMeta{}
	}

	if a.pruneCfg.Enabled {
		// A3: dedup by cache key. If an earlier tool message had the
		// same key, replace its content with a stub now — the new
		// result supersedes it.
		if meta.CacheKey != "" {
			a.dedupCacheKey(meta.CacheKey, msg.Name)
		}
		// A4: any prior read of a now-written path is invalidated.
		if len(meta.PathsWritten) > 0 {
			a.invalidateReadsOf(meta.PathsWritten)
		}
	}

	// Append + persist via the standard path. Note: appendMessage
	// uses len(History) before append for the new index, so capture
	// it before delegating.
	idx := len(a.History)
	a.appendMessage(msg)

	// Sidecar entry for this new tool message.
	tm := &toolMessageMeta{
		toolName:     msg.Name,
		cacheKey:     meta.CacheKey,
		pathsRead:    meta.PathsRead,
		pathsWritten: meta.PathsWritten,
		userTurnAt:   a.userTurnCounter,
	}
	if a.pruneCfg.Enabled && len(a.pruneCfg.PinnedPaths) > 0 {
		for _, p := range meta.PathsRead {
			if pathMatchesPinned(p, a.pruneCfg.PinnedPaths) {
				tm.pinned = true
				break
			}
		}
	}
	a.toolMeta[idx] = tm

	if a.pruneCfg.Enabled {
		// A1/A2: stub anything older than its per-tool threshold.
		// Skips pinned messages and already-stubbed messages.
		a.pruneStaleToolMessages()
	}
}

// dedupCacheKey walks toolMeta for a matching cacheKey and stubs the
// older entry (if any). At most one prior entry per cacheKey is
// expected, but if multiple exist (e.g. pruning was disabled then
// re-enabled), all matches are stubbed.
func (a *Agent) dedupCacheKey(key, toolName string) {
	for idx, tm := range a.toolMeta {
		if tm == nil || tm.stubbed || tm.pinned {
			continue
		}
		if tm.cacheKey != key {
			continue
		}
		if idx >= len(a.History) {
			continue
		}
		a.History[idx].Content = stubFor(stubLabelFor(tm.toolName, toolName), a.History[idx].Content)
		tm.stubbed = true
	}
	a.refreshEstimate()
}

// invalidateReadsOf stubs any prior `read`-class tool message whose
// PathsRead intersects `written`. The model's view of the file is
// stale once the file has been edited, so the verbatim pre-edit
// content in history is actively misleading; replacing with a stub
// frees the tokens AND removes the misinformation.
//
// Pinned reads are spared — if PLAN.md is pinned, an edit to PLAN.md
// still keeps the pre-edit content for now (the assumption is that
// pinned files are sentinel/spec docs the user wants stable; if
// they're being edited mid-session, the user can /prune or unset
// the pin).
func (a *Agent) invalidateReadsOf(written []string) {
	if len(written) == 0 {
		return
	}
	wset := make(map[string]struct{}, len(written))
	for _, p := range written {
		wset[p] = struct{}{}
	}
	for idx, tm := range a.toolMeta {
		if tm == nil || tm.stubbed || tm.pinned {
			continue
		}
		if idx >= len(a.History) {
			continue
		}
		hit := false
		for _, p := range tm.pathsRead {
			if _, ok := wset[p]; ok {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		a.History[idx].Content = stubFor(tm.toolName+"@stale", a.History[idx].Content)
		tm.stubbed = true
	}
	a.refreshEstimate()
}

// pruneStaleToolMessages walks History from the end backwards,
// counting user-message turn boundaries, and stubs tool messages
// whose age exceeds the per-tool retention threshold.
func (a *Agent) pruneStaleToolMessages() {
	if a.pruneCfg.StaleAfter <= 0 && len(a.pruneCfg.ToolRetention) == 0 && len(inCodeDefaultRetention) == 0 {
		return
	}

	turnsBack := 0
	mutated := false
	for i := len(a.History) - 1; i >= 0; i-- {
		m := &a.History[i]
		if m.Role == "user" {
			turnsBack++
			continue
		}
		if m.Role != "tool" {
			continue
		}
		tm := a.toolMeta[i]
		if tm == nil || tm.stubbed || tm.pinned {
			continue
		}
		threshold := a.staleAfterFor(tm.toolName)
		if turnsBack > threshold {
			m.Content = stubFor(tm.toolName, m.Content)
			tm.stubbed = true
			mutated = true
		}
	}
	if mutated {
		a.refreshEstimate()
	}
}

// ForcePrune is the entry point for the /prune slash command and any
// other "compact aggressively, now" trigger. It runs stale stubbing
// with `staleAfter` capped at 1 user-turn — i.e. only the very last
// turn's tool results stay verbatim. Returns the number of messages
// stubbed and the token delta.
func (a *Agent) ForcePrune() (stubbed, beforeTokens, afterTokens int) {
	beforeTokens = llm.Estimate(a.History)
	turnsBack := 0
	for i := len(a.History) - 1; i >= 0; i-- {
		m := &a.History[i]
		if m.Role == "user" {
			turnsBack++
			continue
		}
		if m.Role != "tool" {
			continue
		}
		tm := a.toolMeta[i]
		if tm == nil || tm.stubbed || tm.pinned {
			continue
		}
		// /prune keeps only the most recent user-turn's tool messages.
		if turnsBack >= 1 {
			m.Content = stubFor(tm.toolName, m.Content)
			tm.stubbed = true
			stubbed++
		}
	}
	if stubbed > 0 {
		a.refreshEstimate()
	}
	afterTokens = llm.Estimate(a.History)
	return stubbed, beforeTokens, afterTokens
}

// rebuildToolMeta recomputes the toolMeta index from scratch after
// compaction has shifted History. The synthetic summary message
// added by compaction is not a "tool" message and gets no entry;
// any pinned entries from the older block that survived compaction
// keep their bookkeeping (we re-discover them by walking History
// and matching against the message identity preserved by compaction).
//
// Called from forceCompact after it rewrites a.History.
func (a *Agent) rebuildToolMeta(preserved map[*llm.Message]*toolMessageMeta) {
	next := map[int]*toolMessageMeta{}
	for i := range a.History {
		if a.History[i].Role != "tool" {
			continue
		}
		if tm, ok := preserved[&a.History[i]]; ok && tm != nil {
			next[i] = tm
		}
	}
	a.toolMeta = next
}

// stubLabelFor picks the tool name to display in a dedup stub.
// When old.toolName matches new.toolName we use it as-is; otherwise
// we annotate "X→Y" so the user (and a debug reader) can see one
// tool's output was replaced when a different-named tool reused the
// same cache key (rare but possible if two tools normalise the same
// way).
func stubLabelFor(oldName, newName string) string {
	if oldName == newName || newName == "" {
		return oldName
	}
	return oldName + "→" + newName
}

// PrefixBreakdown is a per-category token-count snapshot of the
// current History. Used by /context and the per-turn debug log so
// users can see where their context budget is going.
//
// Tokens use the standard 4-char heuristic via llm.Estimate so the
// numbers match what /info and the sidebar display.
type PrefixBreakdown struct {
	Total        int // sum of all categories
	System       int // role: "system" message(s)
	Pinned       int // tool messages with toolMeta.pinned == true
	ToolActive   int // tool messages with non-stubbed payload
	ToolStubbed  int // tool messages whose payload has been stubbed
	Conversation int // user + assistant messages
}

// PrefixBreakdown classifies every message in History and returns a
// per-category token count. Safe to call at any time; reads History
// directly so it reflects post-prune state.
func (a *Agent) PrefixBreakdown() PrefixBreakdown {
	var bd PrefixBreakdown
	for i, m := range a.History {
		toks := llm.Estimate([]llm.Message{m})
		bd.Total += toks
		switch m.Role {
		case "system":
			bd.System += toks
		case "tool":
			tm := a.toolMeta[i]
			switch {
			case tm != nil && tm.pinned:
				bd.Pinned += toks
			case tm != nil && tm.stubbed:
				bd.ToolStubbed += toks
			case isStub(m.Content):
				// Defensive: no meta entry but content looks
				// stubbed (e.g. session resume that doesn't
				// rebuild meta). Count as stubbed.
				bd.ToolStubbed += toks
			default:
				bd.ToolActive += toks
			}
		case "user", "assistant":
			bd.Conversation += toks
		default:
			bd.Conversation += toks
		}
	}
	return bd
}
