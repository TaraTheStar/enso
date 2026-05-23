// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"fmt"
	"strings"
)

// truncateWithRecovery applies the configured caps to content and,
// when truncation fires AND the AgentContext carries a spill writer,
// persists the full output to disk and appends a recovery footer
// pointing the model at it.
//
// Returns (modelVisible, fullOutput). The model sees the truncated
// form (possibly with the recovery footer); FullOutput is always the
// untouched input, suitable for persistence and for spill content.
//
// When ac.Spill is nil or the spill write fails, the truncated form
// is returned unchanged — falling back to today's behaviour rather
// than dragging unfetchable recovery noise into the model context.
func truncateWithRecovery(ac *AgentContext, toolName, content string) (model, full string) {
	caps := ac.OutputCaps
	truncated, full := capTruncate(
		content,
		caps.CapFor(toolName),
		caps.BytesFor(toolName),
		caps.LineLengthFor(toolName),
		ac.RecentUserHint,
	)
	if truncated == full {
		return truncated, full
	}
	if ac.Spill == nil {
		return truncated, full
	}
	path, err := ac.Spill.Spill(full)
	if err != nil || path == "" {
		return truncated, full
	}
	return truncated + spillFooter(path), full
}

// spillFooter is the recovery hint the model sees after a truncated
// tool result. Phrasing nudges the model toward the right next call
// (`read` with offset/limit, or `grep` to filter) so it has a clear
// path forward without re-running the original tool.
func spillFooter(path string) string {
	return "\n\n[full output: " + path +
		" — use `read` with offset/limit to recover sections, " +
		"or `grep` to filter]"
}

// HeadTail truncates a string to maxLines, keeping the head and tail.
func HeadTail(s string, maxLines int) (truncated string, full string) {
	full = s
	if maxLines <= 0 {
		maxLines = 2000
	}

	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, s
	}

	half := maxLines / 2
	skipped := len(lines) - maxLines

	parts := make([]string, 0, maxLines+1)
	parts = append(parts, lines[:half]...)
	parts = append(parts, fmt.Sprintf("\n... %d lines truncated ...\n", skipped))
	parts = append(parts, lines[len(lines)-half:]...)

	truncated = strings.Join(parts, "\n")

	return truncated, s
}

// capTruncate is the prune-aware entry point tool implementations use
// instead of HeadTail directly. It applies, in order:
//
//  1. a byte cap (defends against pathological single-line outputs),
//  2. a line cap with optional relevance-based selection,
//  3. a per-line length cap (elides the middle of any long line).
//
// Each cap is independent; zero (or negative) values disable that
// pass. The `full` return is always the untouched input, so callers
// can persist the original (e.g. for spill recovery).
//
// The hint is typically AgentContext.RecentUserHint — the most recent
// user message text. Empty disables the relevance pass; the
// line-cap behaviour is then identical to HeadTail.
func capTruncate(s string, maxLines, maxBytes, maxLineLen int, hint string) (truncated string, full string) {
	full = s
	out := s

	// 1. Byte cap. Catches the "one 100 MB minified-JS line" case the
	//    line cap would happily wave through (one line ≤ 2000 lines).
	if maxBytes > 0 && len(out) > maxBytes {
		out = HeadTailBytes(out, maxBytes)
	}

	// 2. Line cap with optional relevance selection.
	if maxLines <= 0 {
		maxLines = defaultLineCap
	}
	lines := strings.Split(out, "\n")
	if len(lines) > maxLines {
		if h := strings.TrimSpace(hint); h != "" {
			if rel, ok := relevantTruncateLines(lines, maxLines, h); ok {
				out = rel
			} else {
				out, _ = HeadTail(out, maxLines)
			}
		} else {
			out, _ = HeadTail(out, maxLines)
		}
	}

	// 3. Per-line length cap. After the previous two passes the line
	//    count is bounded, but any individual line may still be huge
	//    (think one column of base64 in a CSV). Elide the middle of
	//    each over-long line so the model sees ends only.
	if maxLineLen > 0 {
		out = capLineLengths(out, maxLineLen)
	}

	return out, full
}

// HeadTailBytes truncates by byte budget, keeping the first and last
// halves with a banner showing how many bytes were dropped. Unlike
// HeadTail it operates on raw byte length, not lines — required for
// inputs where a single line exceeds any reasonable line cap.
//
// The split tries to land on a newline boundary near the budget split
// so the result stays line-readable. When no newline is nearby the
// split falls back to the byte index, which can break a UTF-8 rune;
// callers are expected to feed text-ish data anyway.
func HeadTailBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	half := maxBytes / 2
	skipped := len(s) - maxBytes
	head := s[:half]
	tail := s[len(s)-half:]
	// Nudge head to end on a newline if there's one in the last 128
	// bytes; same for tail's start. Keeps the elision banner from
	// landing mid-line.
	if i := strings.LastIndexByte(head[max(0, len(head)-128):], '\n'); i >= 0 {
		head = head[:len(head)-128+i+1]
	}
	if i := strings.IndexByte(tail[:min(len(tail), 128)], '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return head + fmt.Sprintf("\n... %d bytes truncated ...\n", skipped) + tail
}

// capLineLengths walks each line and elides the middle of any line
// longer than maxLen. The banner uses byte counts (not rune counts) so
// it stays cheap; same caveat as HeadTailBytes for multi-byte runes.
func capLineLengths(s string, maxLen int) string {
	lines := strings.Split(s, "\n")
	changed := false
	for i, ln := range lines {
		if len(ln) <= maxLen {
			continue
		}
		half := maxLen / 2
		dropped := len(ln) - maxLen
		lines[i] = ln[:half] + fmt.Sprintf(" ... %d bytes elided ... ", dropped) + ln[len(ln)-half:]
		changed = true
	}
	if !changed {
		return s
	}
	return strings.Join(lines, "\n")
}

// relevantTruncateLines selects up to `maxLines` lines from `lines`
// that score highest against the hint, then renders a banner showing
// how many were dropped. Returns (rendered, true) on success or
// ("", false) when no lines match — caller should fall back to
// head/tail.
//
// Relevance score: number of distinct hint tokens (lowercased,
// length ≥ 3) that appear in the line. Ties broken by line index
// (earlier wins). Each selected line carries 2 lines of bracketing
// context to keep it readable.
func relevantTruncateLines(lines []string, maxLines int, hint string) (string, bool) {
	tokens := relevantHintTokens(hint)
	if len(tokens) == 0 {
		return "", false
	}

	type scored struct {
		idx, score int
	}
	scoredLines := make([]scored, 0, len(lines))
	for i, ln := range lines {
		s := scoreLine(ln, tokens)
		if s > 0 {
			scoredLines = append(scoredLines, scored{idx: i, score: s})
		}
	}
	if len(scoredLines) == 0 {
		return "", false
	}

	// Sort by score desc, then index asc.
	for i := 1; i < len(scoredLines); i++ {
		for j := i; j > 0 && (scoredLines[j-1].score < scoredLines[j].score ||
			(scoredLines[j-1].score == scoredLines[j].score && scoredLines[j-1].idx > scoredLines[j].idx)); j-- {
			scoredLines[j-1], scoredLines[j] = scoredLines[j], scoredLines[j-1]
		}
	}

	// Walk picks in score order, expanding ±2 lines of context
	// around each, until budget is exhausted.
	keep := make(map[int]bool, maxLines)
	const ctx = 2
	for _, s := range scoredLines {
		for d := -ctx; d <= ctx; d++ {
			j := s.idx + d
			if j < 0 || j >= len(lines) {
				continue
			}
			if !keep[j] {
				keep[j] = true
				if len(keep) >= maxLines {
					break
				}
			}
		}
		if len(keep) >= maxLines {
			break
		}
	}
	if len(keep) == 0 {
		return "", false
	}

	// Render in original order, inserting "... N lines ..." between
	// non-contiguous kept blocks.
	var out strings.Builder
	skipped := 0
	prev := -2
	for i := 0; i < len(lines); i++ {
		if !keep[i] {
			skipped++
			continue
		}
		if skipped > 0 && prev >= 0 {
			fmt.Fprintf(&out, "\n... %d non-matching lines elided ...\n", skipped)
		}
		out.WriteString(lines[i])
		out.WriteByte('\n')
		skipped = 0
		prev = i
	}
	if skipped > 0 {
		fmt.Fprintf(&out, "\n... %d trailing non-matching lines elided ...\n", skipped)
	}
	return strings.TrimRight(out.String(), "\n"), true
}

// relevantHintTokens splits the hint into lowercased word-ish tokens
// of at least 3 chars. The min-length filter keeps stopwords ("a",
// "is", "of") from polluting matches.
func relevantHintTokens(hint string) []string {
	fields := strings.FieldsFunc(strings.ToLower(hint), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	})
	seen := make(map[string]struct{}, len(fields))
	out := fields[:0]
	for _, f := range fields {
		if len(f) < 3 {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

func scoreLine(line string, tokens []string) int {
	low := strings.ToLower(line)
	score := 0
	for _, t := range tokens {
		if strings.Contains(low, t) {
			score++
		}
	}
	return score
}
