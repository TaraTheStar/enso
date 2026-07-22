// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"fmt"
	"strings"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/tools"
)

// toolMessageMeta is the per-tool-message bookkeeping the prune
// subsystem (A1–A4, C1) maintains alongside Agent.History. Indexed by
// the message's slot in History; forceCompactWithSeed rebuilds the
// index alongside the rewritten History when compaction shifts slots.
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

// appendToolMessage is the prune-aware append that the agent's
// tool-execution path uses. It:
//
//  1. delegates to appendMessageLocked to persist + grow History,
//  2. records sidecar metadata (cacheKey, paths, age, pinned) for the
//     new message so a later reclaim pass can stub it.
//
// It deliberately does NOT stub anything here. Stubbing rewrites the
// content of OLDER messages, and on a prefix-cached backend any edit to
// an already-sent message invalidates the KV cache from that position to
// the end of the prompt — the earlier the edit, the more expensive. Doing
// it on every tool call (the previous behaviour) meant a steady drip of
// mid-prefix rewrites, each forcing the backend to reprocess the whole
// suffix. The transcript is now append-only between reclaims; all stubbing
// is batched into reclaimToolOutputs, which runs only at the compaction
// boundary (see MaybeCompact), so the cache survives every normal turn and
// the reclaim costs a single invalidation instead of one per tool call.
func (a *Agent) appendToolMessage(msg llm.Message, meta tools.ResultMeta) {
	// One lock hold for the index capture, append, and sidecar write so
	// a concurrent compaction can't shift slots between them.
	a.histMu.Lock()
	defer a.histMu.Unlock()

	// Late-init for backwards-compat — older callers (or tests
	// constructing an Agent without going through New) may not have
	// the map allocated yet.
	if a.toolMeta == nil {
		a.toolMeta = map[int]*toolMessageMeta{}
	}

	// Append + persist via the standard path. Note: the new message's
	// index is len(History) before the append, so capture it first.
	idx := len(a.History)
	a.appendMessageLocked(msg)

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
}

// reclaimToolOutputs runs the in-place tool-output stub reclaim as a
// single batched pass. It is the ONLY place between summary compactions
// where already-sent messages are rewritten, so it is called only at a
// reclaim boundary (MaybeCompact / overflow recovery) — never per tool
// call. That keeps the transcript append-only during normal turns, so the
// backend's prefix KV cache survives, and the reclaim pays one cache
// invalidation here instead of one per tool result.
//
// It folds the three stubbing rules into one sweep:
//   - A3 dedup: an older result superseded by a newer same-cache-key one
//   - A4 stale-read: a read whose path was edited by a later write
//   - A1/A2 age: a result older than its per-tool retention window
//
// Returns the number of messages newly stubbed. Caller must hold
// a.histMu (the passes rewrite History content and read toolMeta).
func (a *Agent) reclaimToolOutputs() int {
	if !a.pruneCfg.Enabled {
		return 0
	}
	stubbed := a.stubSupersededToolOutputs() // A3 + A4
	stubbed += a.pruneStaleToolMessages()    // A1/A2
	return stubbed
}

// stubSupersededToolOutputs stubs tool messages whose content is no
// longer the freshest signal: A3 (an older result with the same cache
// key as a newer one) and A4 (a read whose path was written by a later
// edit, leaving the pre-edit content stale and actively misleading).
//
// Both are computed in a single forward scan: newestForKey holds the
// highest index carrying each cache key, lastWriteIdx the highest index
// that wrote each path. A message is superseded iff a strictly-later
// message supersedes it, so the freshest entry is always kept. Pinned
// reads are spared (sentinel/spec docs the user wants stable).
//
// Caller must hold a.histMu.
func (a *Agent) stubSupersededToolOutputs() int {
	newestForKey := map[string]int{}
	lastWriteIdx := map[string]int{}
	for idx, tm := range a.toolMeta {
		if tm == nil {
			continue
		}
		if tm.cacheKey != "" && idx > newestForKey[tm.cacheKey] {
			newestForKey[tm.cacheKey] = idx
		}
		for _, p := range tm.pathsWritten {
			if idx > lastWriteIdx[p] {
				lastWriteIdx[p] = idx
			}
		}
	}

	stubbed := 0
	for idx, tm := range a.toolMeta {
		if tm == nil || tm.stubbed || tm.pinned || idx >= len(a.History) {
			continue
		}
		// A3: a newer message carries the same cache key.
		if tm.cacheKey != "" && newestForKey[tm.cacheKey] > idx {
			newName := tm.toolName
			if nt := a.toolMeta[newestForKey[tm.cacheKey]]; nt != nil {
				newName = nt.toolName
			}
			a.History[idx].Content = stubFor(stubLabelFor(tm.toolName, newName), a.History[idx].Content)
			tm.stubbed = true
			stubbed++
			continue
		}
		// A4: a path this read was edited by a strictly-later write.
		stale := false
		for _, p := range tm.pathsRead {
			if w, ok := lastWriteIdx[p]; ok && w > idx {
				stale = true
				break
			}
		}
		if stale {
			a.History[idx].Content = stubFor(tm.toolName+"@stale", a.History[idx].Content)
			tm.stubbed = true
			stubbed++
		}
	}
	if stubbed > 0 {
		a.histGen++
		a.refreshEstimate()
	}
	return stubbed
}

// pruneStaleToolMessages walks History from the end backwards,
// counting user-message turn boundaries, and stubs tool messages
// whose age exceeds the per-tool retention threshold. Returns the
// number of messages newly stubbed. Caller must hold a.histMu.
func (a *Agent) pruneStaleToolMessages() int {
	if a.pruneCfg.StaleAfter <= 0 && len(a.pruneCfg.ToolRetention) == 0 && len(inCodeDefaultRetention) == 0 {
		return 0
	}

	turnsBack := 0
	stubbed := 0
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
			stubbed++
		}
	}
	if stubbed > 0 {
		a.histGen++
		a.refreshEstimate()
	}
	return stubbed
}

// ForcePrune is the entry point for the /prune slash command and any
// other "compact aggressively, now" trigger. It runs stale stubbing
// with `staleAfter` capped at 1 user-turn — i.e. only the very last
// turn's tool results stay verbatim. Returns the number of messages
// stubbed and the token delta. Safe to call from any goroutine
// (control RPCs / slash commands) — it takes the history lock.
func (a *Agent) ForcePrune() (stubbed, beforeTokens, afterTokens int) {
	a.histMu.Lock()
	defer a.histMu.Unlock()
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
		a.histGen++
		a.refreshEstimate()
	}
	afterTokens = llm.Estimate(a.History)
	return stubbed, beforeTokens, afterTokens
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

// CompressionSaved returns the cumulative tokens saved this session by
// output compression + truncation (H11). 0 when compression is disabled or
// nothing has been trimmed yet. Surfaced by /context.
func (a *Agent) CompressionSaved() int64 {
	return a.compression.Saved()
}

// PrefixBreakdown classifies every message in History and returns a
// per-category token count. Safe to call from any goroutine (control
// RPCs / slash commands / the per-turn debug log) — it takes the
// history lock and reads History directly, so it reflects post-prune
// state.
func (a *Agent) PrefixBreakdown() PrefixBreakdown {
	a.histMu.Lock()
	defer a.histMu.Unlock()
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
