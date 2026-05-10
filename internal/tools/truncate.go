// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"fmt"
	"strings"
)

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

// capTruncate is the prune-aware entry point tool implementations
// use instead of HeadTail directly. When `hint` is non-empty AND the
// input exceeds the cap, RelevantTruncate is used to keep lines that
// match the hint. Otherwise (or if the relevance pass selects
// nothing useful) it falls back to HeadTail.
//
// The hint is typically AgentContext.RecentUserHint — the most
// recent user message text. Empty disables the relevance pass; the
// behaviour is then identical to HeadTail.
func capTruncate(s string, maxLines int, hint string) (truncated string, full string) {
	if maxLines <= 0 {
		maxLines = 2000
	}
	full = s
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, s
	}
	if h := strings.TrimSpace(hint); h != "" {
		if rel, ok := relevantTruncateLines(lines, maxLines, h); ok {
			return rel, s
		}
	}
	return HeadTail(s, maxLines)
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
